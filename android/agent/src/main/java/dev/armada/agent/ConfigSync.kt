package dev.armada.agent

import android.app.admin.DevicePolicyManager
import android.app.admin.SystemUpdatePolicy
import android.content.ComponentName
import android.content.Context
import android.os.Bundle
import android.util.Log
import org.json.JSONArray
import org.json.JSONObject
import java.time.Instant

/**
 * Applies configuration profiles on Android: app managed configurations
 * (the "app restrictions" bundle an app reads with RestrictionsManager) and
 * the OS system-update policy. Mirrors the Linux agent's protocol: report
 * every key with its hash and status; apply items whose hash we lack;
 * remove keys the server no longer wants.
 */
class ConfigSync(private val context: Context, private val prefs: Prefs) {
    private val dpm = context.getSystemService(DevicePolicyManager::class.java)
    private val admin = ComponentName(context, AdminReceiver::class.java)

    /** The heartbeat's "config" list. Never null, so the server knows we support profiles. */
    fun report(): JSONArray {
        val out = JSONArray()
        val state = prefs.configState
        for (key in state.keys()) out.put(JSONObject(state.getJSONObject(key).toString()).put("key", key))
        return out
    }

    @Synchronized
    fun apply(sync: JSONObject) {
        val desired = sync.optJSONObject("desired") ?: JSONObject()
        val state = prefs.configState
        for (key in state.keys().asSequence().toList()) {
            if (!desired.has(key)) {
                runCatching { remove(key) }.onFailure { Log.w(Agent.TAG, "removing configuration $key failed", it) }
                state.remove(key)
            }
        }
        val items = sync.optJSONArray("items") ?: JSONArray()
        for (i in 0 until items.length()) {
            val it = items.getJSONObject(i)
            val key = it.getString("key")
            val hash = it.getString("hash")
            if (desired.optString(key) != hash) continue
            val prev = state.optJSONObject(key)
            if (prev != null && prev.optString("hash") == hash && prev.optString("status") == "applied") continue
            val entry = JSONObject().put("hash", hash).put("profile_id", it.optString("profile_id"))
                .put("updated_at", Instant.now().toString())
            try {
                applyItem(it)
                entry.put("status", "applied")
                Log.i(Agent.TAG, "configuration $key applied")
            } catch (e: Exception) {
                entry.put("status", "failed").put("message", e.message ?: e.toString())
                Log.w(Agent.TAG, "configuration $key failed", e)
            }
            state.put(key, entry)
        }
        prefs.configState = state
    }

    private fun applyItem(it: JSONObject) {
        when (it.getString("kind")) {
            "android_app" -> dpm.setApplicationRestrictions(admin, it.getString("target"), toBundle(it.optJSONObject("values") ?: JSONObject()))
            "android_system_update" -> dpm.setSystemUpdatePolicy(admin, systemUpdatePolicy(it.getJSONObject("system_update")))
            else -> throw IllegalArgumentException("configuration kind ${it.getString("kind")} is not supported on Android")
        }
    }

    private fun remove(key: String) {
        val kind = key.substringBefore(':')
        when (kind) {
            "android_app" -> dpm.setApplicationRestrictions(admin, key.substringAfter(':'), Bundle())
            "android_system_update" -> dpm.setSystemUpdatePolicy(admin, null)
        }
    }

    companion object {
        /** Converts JSON values to a restrictions bundle (string, boolean, integer, string list). */
        fun toBundle(values: JSONObject): Bundle {
            val b = Bundle()
            for (k in values.keys()) {
                when (val v = values.get(k)) {
                    is String -> b.putString(k, v)
                    is Boolean -> b.putBoolean(k, v)
                    is Int -> b.putInt(k, v)
                    is Long -> if (v in Int.MIN_VALUE..Int.MAX_VALUE) b.putInt(k, v.toInt()) else b.putString(k, v.toString())
                    is Double -> if (v == Math.floor(v) && v in Int.MIN_VALUE.toDouble()..Int.MAX_VALUE.toDouble()) b.putInt(k, v.toInt()) else b.putString(k, v.toString())
                    is JSONArray -> b.putStringArray(k, Array(v.length()) { v.getString(it) })
                    else -> b.putString(k, v.toString())
                }
            }
            return b
        }

        fun minutes(hhmm: String): Int {
            val (h, m) = hhmm.split(':').map { it.toInt() }
            return h * 60 + m
        }

        fun systemUpdatePolicy(p: JSONObject): SystemUpdatePolicy = when (p.getString("type")) {
            "automatic" -> SystemUpdatePolicy.createAutomaticInstallPolicy()
            "postpone" -> SystemUpdatePolicy.createPostponeInstallPolicy()
            "windowed" -> SystemUpdatePolicy.createWindowedInstallPolicy(minutes(p.getString("window_start")), minutes(p.getString("window_end")))
            else -> throw IllegalArgumentException("unknown system update policy ${p.getString("type")}")
        }
    }
}
