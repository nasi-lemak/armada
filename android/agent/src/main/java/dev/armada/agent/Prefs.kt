package dev.armada.agent

import android.annotation.SuppressLint
import android.content.Context
import android.content.SharedPreferences
import org.json.JSONArray
import org.json.JSONObject

/**
 * Agent state. Stored in device-protected storage so the agent can check in
 * after a reboot before the user unlocks the device. Backups are disabled in
 * the manifest, so this never leaves the device.
 */
class Prefs(context: Context) {
    private val sp: SharedPreferences =
        context.createDeviceProtectedStorageContext().getSharedPreferences("armada", Context.MODE_PRIVATE)

    var serverUrl: String?
        get() = sp.getString("server_url", null)
        set(v) = sp.edit().putString("server_url", v?.trimEnd('/')).apply()

    var enrollmentToken: String?
        get() = sp.getString("enrollment_token", null)
        set(v) = sp.edit().putString("enrollment_token", v).apply()

    var deviceId: String?
        get() = sp.getString("device_id", null)
        set(v) = sp.edit().putString("device_id", v).apply()

    var deviceKey: String?
        get() = sp.getString("device_key", null)
        set(v) = sp.edit().putString("device_key", v).apply()

    /** Base64 Ed25519 key that signs releases; trusted from enrollment onwards. */
    var releaseSigningKey: String?
        get() = sp.getString("release_signing_key", null)
        set(v) = sp.edit().putString("release_signing_key", v).apply()

    var heartbeatSeconds: Int
        get() = sp.getInt("heartbeat_seconds", 30)
        set(v) = sp.edit().putInt("heartbeat_seconds", v).apply()

    var managedPackages: List<String>
        get() = sp.getString("managed_packages", null)?.let { s ->
            val a = JSONArray(s)
            List(a.length()) { a.getString(it) }
        } ?: emptyList()
        set(v) = sp.edit().putString("managed_packages", JSONArray(v).toString()).apply()

    var disableDebugging: Boolean
        get() = sp.getBoolean("disable_debugging", true)
        set(v) = sp.edit().putBoolean("disable_debugging", v).apply()

    var blockUninstall: Boolean
        get() = sp.getBoolean("block_uninstall", true)
        set(v) = sp.edit().putBoolean("block_uninstall", v).apply()

    /** A self-update in flight: {deployment_id, release_id, version}. Written
     *  synchronously: the install kills this process moments later. */
    @get:Synchronized @set:Synchronized
    var pendingSelfUpdate: JSONObject?
        get() = sp.getString("pending_self_update", null)?.let { JSONObject(it) }
        @SuppressLint("ApplySharedPref")
        set(v) {
            sp.edit().putString("pending_self_update", v?.toString()).commit()
        }

    /** Update reports that could not be delivered yet. Written with commit() so
     *  they survive the process being killed by a self-update right after. */
    @SuppressLint("ApplySharedPref")
    @Synchronized
    fun addToOutbox(report: JSONObject) {
        val a = JSONArray(sp.getString("outbox", "[]"))
        a.put(report)
        sp.edit().putString("outbox", a.toString()).commit()
    }

    @SuppressLint("ApplySharedPref")
    @Synchronized
    fun takeOutbox(): List<JSONObject> {
        val a = JSONArray(sp.getString("outbox", "[]"))
        sp.edit().putString("outbox", "[]").commit()
        return List(a.length()) { a.getJSONObject(it) }
    }

    val isEnrolled: Boolean get() = !deviceKey.isNullOrEmpty() && !deviceId.isNullOrEmpty()
}
