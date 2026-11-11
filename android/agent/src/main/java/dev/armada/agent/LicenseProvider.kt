package dev.armada.agent

import android.content.ContentProvider
import android.content.ContentValues
import android.content.pm.PackageManager
import android.database.Cursor
import android.net.Uri
import android.os.Binder
import android.os.Bundle
import android.util.Log
import org.json.JSONObject
import java.util.concurrent.ConcurrentHashMap

/**
 * Apps on the device call `ContentResolver.call(content://dev.armada.agent.license, "getLicense", null, extras)`
 * (the client library does this) to get a license for themselves.
 *
 * The caller cannot lie about who it is: the package comes from its Binder
 * UID and the signing certificate from the package manager. The server only
 * issues a license if that package and certificate are registered, the device
 * is enrolled with an attested hardware key, and its licenses are not revoked.
 */
class LicenseProvider : ContentProvider() {
    private val cache = ConcurrentHashMap<String, JSONObject>()

    override fun call(method: String, arg: String?, extras: Bundle?): Bundle {
        if (method != "getLicense") return error("unknown method $method")
        val ctx = context ?: return error("no context")
        val uid = Binder.getCallingUid()
        val pkg = ctx.packageManager.getPackagesForUid(uid)?.firstOrNull() ?: return error("unknown caller")
        val signer = try {
            @Suppress("DEPRECATION")
            val info = ctx.packageManager.getPackageInfo(pkg, PackageManager.GET_SIGNING_CERTIFICATES)
            Updater.signerDigests(info).firstOrNull() ?: return error("caller has no signing certificate")
        } catch (e: PackageManager.NameNotFoundException) {
            return error("unknown caller")
        }
        val jwk = extras?.getString("app_key_jwk")?.let { runCatching { JSONObject(it) }.getOrNull() }
        val cacheKey = "$pkg|$signer|${jwk?.let { Agent.thumbprint(it) }.orEmpty()}"
        val forceRefresh = extras?.getBoolean("refresh") == true

        val token = Binder.clearCallingIdentity()
        try {
            val cached = cache[cacheKey]
            if (!forceRefresh && cached != null && expiresAt(cached) - System.currentTimeMillis() > 5 * 60_000) {
                return ok(cached)
            }
            val lic = Agent.get(ctx).requestLicense(pkg, signer, jwk)
            cache[cacheKey] = lic
            return ok(lic)
        } catch (e: ApiException) {
            Log.w(Agent.TAG, "license for $pkg refused: ${e.status} ${e.message}")
            return error(e.message ?: "license refused", e.status)
        } catch (e: Exception) {
            Log.w(Agent.TAG, "license for $pkg failed", e)
            return error(e.message ?: e.toString())
        } finally {
            Binder.restoreCallingIdentity(token)
        }
    }

    private fun expiresAt(lic: JSONObject): Long =
        runCatching { java.time.Instant.parse(lic.getString("expires_at")).toEpochMilli() }.getOrDefault(0L)

    private fun ok(lic: JSONObject) = Bundle().apply {
        putString("license", lic.getString("license"))
        putLong("expires_at", expiresAt(lic))
    }

    private fun error(msg: String, status: Int = 0) = Bundle().apply {
        putString("error", msg)
        putInt("status", status)
    }

    override fun onCreate() = true
    override fun query(uri: Uri, projection: Array<String>?, selection: String?, selectionArgs: Array<String>?, sortOrder: String?): Cursor? = null
    override fun getType(uri: Uri): String? = null
    override fun insert(uri: Uri, values: ContentValues?): Uri? = null
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<String>?) = 0
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<String>?) = 0
}
