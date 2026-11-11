package dev.armada.agent

import android.app.admin.DeviceAdminReceiver
import android.app.admin.DevicePolicyManager
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.PersistableBundle

/** Device Owner entry point. Provisioning hands us the server URL and enrollment token. */
class AdminReceiver : DeviceAdminReceiver() {
    override fun onProfileProvisioningComplete(context: Context, intent: Intent) {
        saveProvisioningExtras(context, intent)
        AgentService.start(context)
    }

    override fun onEnabled(context: Context, intent: Intent) {
        AgentService.start(context)
    }

    companion object {
        /** Stores server_url and enrollment_token from the QR code's admin extras bundle. */
        fun saveProvisioningExtras(context: Context, intent: Intent) {
            val extras: PersistableBundle = if (Build.VERSION.SDK_INT >= 33) {
                intent.getParcelableExtra(DevicePolicyManager.EXTRA_PROVISIONING_ADMIN_EXTRAS_BUNDLE, PersistableBundle::class.java)
            } else {
                @Suppress("DEPRECATION")
                intent.getParcelableExtra(DevicePolicyManager.EXTRA_PROVISIONING_ADMIN_EXTRAS_BUNDLE)
            } ?: return
            val prefs = Prefs(context)
            extras.getString("server_url")?.let { if (it.isNotBlank()) prefs.serverUrl = it }
            extras.getString("enrollment_token")?.let { if (it.isNotBlank() && !prefs.isEnrolled) prefs.enrollmentToken = it }
        }
    }
}
