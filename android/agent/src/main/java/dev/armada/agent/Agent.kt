package dev.armada.agent

import android.annotation.SuppressLint
import android.app.ActivityManager
import android.app.admin.DevicePolicyManager
import android.content.ComponentName
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import android.os.StatFs
import android.os.SystemClock
import android.os.UserManager
import android.util.Base64
import android.util.Log
import org.json.JSONArray
import org.json.JSONObject
import java.net.Inet4Address
import java.net.NetworkInterface
import java.security.MessageDigest

/**
 * The agent's logic, shared by the background service and the license
 * provider. All methods block; call them off the main thread.
 */
class Agent private constructor(private val context: Context) {
    val prefs = Prefs(context)
    private val dpm = context.getSystemService(DevicePolicyManager::class.java)
    private val admin = ComponentName(context, AdminReceiver::class.java)

    private fun api(): ApiClient {
        val url = prefs.serverUrl ?: throw IllegalStateException("server URL not configured; provision the device with the Armada QR code")
        return ApiClient(url) { prefs.deviceKey }
    }

    val isDeviceOwner: Boolean get() = dpm.isDeviceOwnerApp(context.packageName)

    // ---- enrollment ----

    @Synchronized
    fun ensureEnrolled() {
        if (prefs.isEnrolled) return
        val token = prefs.enrollmentToken ?: throw IllegalStateException("not enrolled and no enrollment token")
        val api = api()
        val challenge = api.post("/api/v1/agent/challenge", null, auth = false).getString("challenge")
        val chain = HardwareKey.generate(context, Base64.decode(challenge, Base64.DEFAULT))
        val body = JSONObject()
            .put("token", token)
            .put("name", deviceName())
            .put("info", info())
            .put("attestation", JSONObject().put("challenge", challenge).put("chain", JSONArray(chain)))
        val r = api.post("/api/v1/agent/enroll", body, auth = false)
        prefs.deviceId = r.getString("device_id")
        prefs.deviceKey = r.getString("device_key")
        prefs.releaseSigningKey = r.getString("signing_public_key")
        prefs.heartbeatSeconds = r.optInt("heartbeat_interval_seconds", 30)
        prefs.enrollmentToken = null // single purpose; don't keep it around
        Log.i(TAG, "enrolled as ${prefs.deviceId}")
    }

    // ---- check-in ----

    /** One check-in: report state, apply policy, install any offered update, run actions. */
    fun checkIn() {
        ensureEnrolled()
        confirmSelfUpdate()
        flushOutbox()
        val body = JSONObject()
            .put("info", info())
            .put("metrics", metrics())
            .put("packages", packageVersions())
            .put("busy", false)
        val r = api().post("/api/v1/agent/heartbeat", body)
        prefs.heartbeatSeconds = r.optInt("heartbeat_interval_seconds", prefs.heartbeatSeconds)
        r.optJSONArray("managed_packages")?.let { a -> prefs.managedPackages = List(a.length()) { a.getString(it) } }
        r.optJSONObject("policy")?.let {
            prefs.disableDebugging = it.optBoolean("disable_debugging", true)
            prefs.blockUninstall = it.optBoolean("block_uninstall", true)
        }
        applyPolicy()
        r.optJSONObject("update")?.let { Updater(context, this).apply(it) }
        r.optJSONArray("actions")?.let { a -> for (i in 0 until a.length()) runAction(a.getJSONObject(i)) }
    }

    fun applyPolicy() {
        if (!isDeviceOwner) return
        try {
            if (prefs.disableDebugging) dpm.addUserRestriction(admin, UserManager.DISALLOW_DEBUGGING_FEATURES)
            else dpm.clearUserRestriction(admin, UserManager.DISALLOW_DEBUGGING_FEATURES)
            for (pkg in prefs.managedPackages) {
                if (pkg != context.packageName && isInstalled(pkg)) dpm.setUninstallBlocked(admin, pkg, prefs.blockUninstall)
            }
            if (Build.VERSION.SDK_INT >= 33) {
                // Let the foreground-service notification show without asking the user.
                dpm.setPermissionGrantState(admin, context.packageName, android.Manifest.permission.POST_NOTIFICATIONS,
                    DevicePolicyManager.PERMISSION_GRANT_STATE_GRANTED)
            }
        } catch (e: SecurityException) {
            Log.w(TAG, "could not apply policy", e)
        }
    }

    // ---- update reports ----

    fun report(deploymentId: String, releaseId: String, status: String, message: String = "") {
        val rep = JSONObject().put("deployment_id", deploymentId).put("release_id", releaseId)
            .put("status", status).put("message", message)
        try {
            api().post("/api/v1/agent/updates", rep)
        } catch (e: ApiException) {
            if (e.status >= 500) prefs.addToOutbox(rep) // server trouble: retry later
        } catch (e: Exception) {
            prefs.addToOutbox(rep) // offline: retry later
        }
    }

    private fun flushOutbox() {
        for (rep in prefs.takeOutbox()) {
            try {
                api().post("/api/v1/agent/updates", rep)
            } catch (e: ApiException) {
                if (e.status >= 500) prefs.addToOutbox(rep)
            } catch (e: Exception) {
                prefs.addToOutbox(rep)
            }
        }
    }

    /** After installing a new agent APK the process restarts; confirm or fail the update. */
    private fun confirmSelfUpdate() {
        val p = prefs.pendingSelfUpdate ?: return
        prefs.pendingSelfUpdate = null
        val ok = p.optString("version") == BuildConfig.VERSION_NAME
        prefs.addToOutbox(
            JSONObject().put("deployment_id", p.getString("deployment_id")).put("release_id", p.getString("release_id"))
                .put("status", if (ok) "succeeded" else "failed")
                .put("message", if (ok) "agent updated" else "agent still running ${BuildConfig.VERSION_NAME}")
        )
    }

    // ---- actions ----

    private fun runAction(a: JSONObject) {
        val id = a.getString("id")
        fun result(status: String, output: String, code: Int = 0) = runCatching {
            api().post("/api/v1/agent/actions/$id/result", JSONObject().put("status", status).put("output", output).put("exit_code", code))
        }
        when (a.getString("type")) {
            "reboot" -> {
                if (!isDeviceOwner) return run { result("rejected", "agent is not device owner", -1) }
                result("succeeded", "rebooting")
                dpm.reboot(admin)
            }
            "restart-agent" -> {
                result("succeeded", "restarting agent")
                AgentService.restart(context)
            }
            else -> result("rejected", "action ${a.getString("type")} is not supported on Android", -1)
        }
    }

    // ---- licensing ----

    /**
     * Requests a license for [callerPackage] (identified by the provider from
     * its Binder UID). The request is signed by the hardware key, which the
     * server checks against the key attested at enrollment.
     */
    fun requestLicense(callerPackage: String, callerSigner: String, appKeyJwk: JSONObject?): JSONObject {
        ensureEnrolled()
        val api = api()
        val challenge = api.post("/api/v1/agent/challenge", null, auth = false).getString("challenge")
        val jkt = appKeyJwk?.let { thumbprint(it) } ?: ""
        val input = "armada-license-request-v1\n${prefs.deviceId}\n$challenge\n$callerPackage\n$callerSigner\n$jkt\n"
        val sig = Base64.encodeToString(HardwareKey.sign(input.toByteArray()), Base64.NO_WRAP)
        val body = JSONObject()
            .put("app_package", callerPackage)
            .put("app_signer", callerSigner)
            .put("challenge", challenge)
            .put("signature", sig)
        if (appKeyJwk != null) body.put("app_key_jwk", appKeyJwk)
        return api.post("/api/v1/agent/license", body)
    }

    // ---- device facts ----

    fun isInstalled(pkg: String): Boolean = try {
        context.packageManager.getPackageInfo(pkg, 0); true
    } catch (e: PackageManager.NameNotFoundException) {
        false
    }

    fun installedVersion(pkg: String): Pair<String, Long>? = try {
        val pi = context.packageManager.getPackageInfo(pkg, 0)
        (pi.versionName ?: "") to pi.longVersionCode
    } catch (e: PackageManager.NameNotFoundException) {
        null
    }

    private fun packageVersions(): JSONObject {
        val out = JSONObject().put(context.packageName, BuildConfig.VERSION_NAME)
        for (pkg in prefs.managedPackages) installedVersion(pkg)?.let { out.put(pkg, it.first) }
        return out
    }

    private fun deviceName(): String = "${Build.MANUFACTURER} ${Build.MODEL}".trim()

    private fun info(): JSONObject = JSONObject()
        .put("hostname", deviceName())
        .put("os", "android")
        .put("arch", goArch(Build.SUPPORTED_ABIS.firstOrNull().orEmpty()))
        .put("kernel", "Android ${Build.VERSION.RELEASE} (API ${Build.VERSION.SDK_INT}, patch ${Build.VERSION.SECURITY_PATCH})")
        .put("agent_version", BuildConfig.VERSION_NAME)
        .put("ips", JSONArray(localIps()))

    private fun metrics(): JSONObject {
        val mem = ActivityManager.MemoryInfo().also { context.getSystemService(ActivityManager::class.java).getMemoryInfo(it) }
        val fs = StatFs(context.filesDir.absolutePath)
        return JSONObject()
            .put("uptime_seconds", SystemClock.elapsedRealtime() / 1000)
            .put("num_cpu", Runtime.getRuntime().availableProcessors())
            .put("mem_total_bytes", mem.totalMem)
            .put("mem_available_bytes", mem.availMem)
            .put("disk_total_bytes", fs.totalBytes)
            .put("disk_free_bytes", fs.availableBytes)
            .put("disk_used_bytes", fs.totalBytes - fs.freeBytes)
    }

    private fun localIps(): List<String> = runCatching {
        NetworkInterface.getNetworkInterfaces().toList()
            .filter { it.isUp && !it.isLoopback }
            .flatMap { it.inetAddresses.toList() }
            .filter { it is Inet4Address || !it.isLinkLocalAddress }
            .mapNotNull { it.hostAddress?.substringBefore('%') }
            .sorted()
    }.getOrDefault(emptyList())

    companion object {
        const val TAG = "ArmadaAgent"

        // Holds only the application context, so this cannot leak an Activity.
        @SuppressLint("StaticFieldLeak")
        @Volatile private var instance: Agent? = null

        fun get(context: Context): Agent =
            instance ?: synchronized(this) { instance ?: Agent(context.applicationContext).also { instance = it } }

        fun goArch(abi: String) = when (abi) {
            "arm64-v8a" -> "arm64"
            "armeabi-v7a", "armeabi" -> "arm"
            "x86_64" -> "amd64"
            "x86" -> "386"
            else -> abi
        }

        /** RFC 7638 thumbprint of an EC JWK, matching the server's computation. */
        fun thumbprint(jwk: JSONObject): String {
            val canon = "{\"crv\":\"${jwk.getString("crv")}\",\"kty\":\"${jwk.getString("kty")}\"," +
                "\"x\":\"${jwk.getString("x")}\",\"y\":\"${jwk.getString("y")}\"}"
            val sum = MessageDigest.getInstance("SHA-256").digest(canon.toByteArray())
            return java.util.Base64.getUrlEncoder().withoutPadding().encodeToString(sum)
        }

        fun sha256Hex(b: ByteArray): String =
            MessageDigest.getInstance("SHA-256").digest(b).joinToString("") { "%02x".format(it) }
    }
}
