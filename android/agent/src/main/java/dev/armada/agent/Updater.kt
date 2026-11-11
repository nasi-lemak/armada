package dev.armada.agent

import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInfo
import android.content.pm.PackageInstaller
import android.content.pm.PackageManager
import android.content.pm.Signature
import android.os.Build
import android.util.Base64
import android.util.Log
import com.google.crypto.tink.subtle.Ed25519Verify
import org.json.JSONObject
import java.io.File
import java.security.MessageDigest
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit

/**
 * Installs a release offered by the server.
 *
 * Every APK is checked twice before installing: the server's Ed25519 release
 * signature and SHA-256 (so the file is exactly what was uploaded), and the
 * APK itself (package name, versionCode, and that it is signed by one of the
 * certificates registered for the app). Android then enforces that updates
 * keep the same signer and a higher versionCode.
 */
class Updater(private val context: Context, private val agent: Agent) {

    fun apply(u: JSONObject) {
        val deploymentId = u.getString("deployment_id")
        val releaseId = u.getString("release_id")
        val pkg = u.getString("package")
        val version = u.getString("version")
        fun report(status: String, msg: String = "") {
            Log.i(TAG, "$pkg $version: $status $msg")
            agent.report(deploymentId, releaseId, status, msg)
        }
        report("downloading")
        val apk = try {
            fetch(u)
        } catch (e: Exception) {
            return report("failed", e.message ?: e.toString())
        }
        try {
            verifyApk(apk, u)
        } catch (e: Exception) {
            apk.delete()
            return report("failed", e.message ?: e.toString())
        }
        report("installing")
        val self = pkg == context.packageName
        if (self) {
            // This process is killed when the new agent replaces it; the new
            // process reports the result (see Agent.confirmSelfUpdate).
            agent.prefs.pendingSelfUpdate = JSONObject().put("deployment_id", deploymentId)
                .put("release_id", releaseId).put("version", version)
        }
        val (ok, message) = try {
            install(apk, pkg)
        } catch (e: Exception) {
            false to (e.message ?: e.toString())
        } finally {
            apk.delete()
        }
        if (self) agent.prefs.pendingSelfUpdate = null
        if (!ok) return report("failed", message)
        val installed = agent.installedVersion(pkg)
        if (installed == null || installed.first != version) {
            return report("failed", "installer reported success but ${installed?.first ?: "nothing"} is installed")
        }
        agent.applyPolicy()
        report("succeeded", "installed $version (versionCode ${installed.second})")
    }

    private fun fetch(u: JSONObject): File {
        val pkg = u.getString("package")
        val version = u.getString("version")
        val sha = u.getString("sha256")
        val size = u.getLong("size")
        val keyB64 = agent.prefs.releaseSigningKey ?: error("no release signing key; re-enroll the device")
        val payload = "armada-release-v1\n$pkg\n$version\n$sha\n".toByteArray()
        try {
            Ed25519Verify(Base64.decode(keyB64, Base64.DEFAULT))
                .verify(Base64.decode(u.getString("signature"), Base64.DEFAULT), payload)
        } catch (e: Exception) {
            error("release signature is invalid; refusing to install")
        }
        val dir = File(context.noBackupFilesDir, "downloads").apply { mkdirs() }
        val file = File(dir, u.getString("release_id").replace(Regex("[^A-Za-z0-9_]"), "_") + ".apk")
        ApiClient(agent.prefs.serverUrl!!) { agent.prefs.deviceKey }.download(u.getString("artifact_path"), file, size)
        val actual = sha256Hex(file)
        if (file.length() != size || actual != sha) {
            file.delete()
            error("download checksum mismatch")
        }
        return file
    }

    private fun verifyApk(apk: File, u: JSONObject) {
        val pkg = u.getString("package")
        val signers = u.optJSONArray("signers")?.let { a -> List(a.length()) { a.getString(it).lowercase().replace(":", "") } }.orEmpty()
        if (signers.isEmpty()) error("$pkg is not registered with signing certificates on the server; refusing to install")
        val info = archiveInfo(apk) ?: error("file is not a valid APK")
        if (info.packageName != pkg) error("APK is ${info.packageName}, expected $pkg")
        val versionCode = u.optLong("version_code", 0)
        if (versionCode > 0 && info.longVersionCode != versionCode) error("APK versionCode is ${info.longVersionCode}, expected $versionCode")
        if (info.versionName != u.getString("version")) error("APK versionName is ${info.versionName}, expected ${u.getString("version")}")
        val apkSigners = signerDigests(info)
        if (apkSigners.none { it in signers }) error("APK is not signed with a registered certificate")
        agent.installedVersion(pkg)?.let { (_, code) ->
            if (info.longVersionCode <= code) error("APK versionCode ${info.longVersionCode} is not newer than installed $code (Android cannot downgrade apps)")
        }
    }

    @Suppress("DEPRECATION")
    private fun archiveInfo(apk: File): PackageInfo? {
        val pm = context.packageManager
        val flags = PackageManager.GET_SIGNING_CERTIFICATES
        return if (Build.VERSION.SDK_INT >= 33) {
            pm.getPackageArchiveInfo(apk.path, PackageManager.PackageInfoFlags.of(flags.toLong()))
        } else {
            pm.getPackageArchiveInfo(apk.path, flags)
        }
    }

    private fun install(apk: File, pkg: String): Pair<Boolean, String> {
        if (!agent.isDeviceOwner) return false to "agent is not device owner, so it cannot install silently"
        val installer = context.packageManager.packageInstaller
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL).apply {
            setAppPackageName(pkg)
            if (Build.VERSION.SDK_INT >= 31) setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED)
        }
        val sessionId = installer.createSession(params)
        val latch = CountDownLatch(1)
        results[sessionId] = Result(latch)
        try {
            installer.openSession(sessionId).use { session ->
                session.openWrite("base.apk", 0, apk.length()).use { out ->
                    apk.inputStream().use { it.copyTo(out) }
                    session.fsync(out)
                }
                val intent = Intent(context, InstallResultReceiver::class.java).putExtra(EXTRA_SESSION, sessionId)
                val pi = PendingIntent.getBroadcast(context, sessionId, intent,
                    PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_MUTABLE)
                session.commit(pi.intentSender)
            }
            if (!latch.await(10, TimeUnit.MINUTES)) return false to "installer did not respond within 10 minutes"
            val r = results[sessionId]!!
            return when (r.status) {
                PackageInstaller.STATUS_SUCCESS -> true to "installed"
                PackageInstaller.STATUS_PENDING_USER_ACTION -> false to "installation needs user confirmation (is the agent device owner?)"
                else -> false to "install failed (status ${r.status}): ${r.message ?: "no details"}"
            }
        } finally {
            results.remove(sessionId)
        }
    }

    class Result(val latch: CountDownLatch) {
        @Volatile var status: Int = PackageInstaller.STATUS_FAILURE
        @Volatile var message: String? = null
    }

    companion object {
        private const val TAG = "ArmadaUpdater"
        const val EXTRA_SESSION = "dev.armada.agent.SESSION"
        val results = ConcurrentHashMap<Int, Result>()

        fun sha256Hex(f: File): String {
            val md = MessageDigest.getInstance("SHA-256")
            f.inputStream().use { input ->
                val buf = ByteArray(64 * 1024)
                while (true) {
                    val n = input.read(buf)
                    if (n < 0) break
                    md.update(buf, 0, n)
                }
            }
            return md.digest().joinToString("") { "%02x".format(it) }
        }

        @Suppress("DEPRECATION")
        fun signerDigests(info: PackageInfo): List<String> {
            val si = info.signingInfo
            val sigs: Array<Signature> = when {
                si == null -> info.signatures ?: emptyArray()
                si.hasMultipleSigners() -> si.apkContentsSigners
                else -> si.signingCertificateHistory?.takeLast(1)?.toTypedArray() ?: emptyArray()
            }
            return sigs.map { Agent.sha256Hex(it.toByteArray()) }
        }
    }
}
