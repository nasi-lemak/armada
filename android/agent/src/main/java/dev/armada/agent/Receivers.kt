package dev.armada.agent

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller

/** Restarts the agent after boot and after it updates itself. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        when (intent.action) {
            Intent.ACTION_BOOT_COMPLETED, Intent.ACTION_MY_PACKAGE_REPLACED -> {
                if (Prefs(context).serverUrl != null) AgentService.start(context)
            }
        }
    }
}

/** Receives PackageInstaller results and wakes the waiting Updater. */
class InstallResultReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val session = intent.getIntExtra(Updater.EXTRA_SESSION, -1)
        val r = Updater.results[session] ?: return
        r.status = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)
        r.message = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE)
        r.latch.countDown()
    }
}
