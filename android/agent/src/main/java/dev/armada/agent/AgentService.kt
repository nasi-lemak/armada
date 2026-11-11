package dev.armada.agent

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.util.Log
import kotlin.concurrent.thread
import kotlin.math.min

/**
 * Keeps the agent checking in. A foreground service with a low-priority
 * notification is the reliable way to run periodically on a managed device;
 * Device Owner apps are exempt from the background-start limits that would
 * otherwise apply.
 */
class AgentService : Service() {
    @Volatile private var running = false
    private var worker: Thread? = null

    override fun onCreate() {
        super.onCreate()
        startInForeground()
        running = true
        worker = thread(name = "armada-agent") { loop() }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int = START_STICKY

    override fun onDestroy() {
        running = false
        worker?.interrupt()
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun loop() {
        val agent = Agent.get(this)
        var backoffSec = 5L
        while (running) {
            val wait = try {
                agent.checkIn()
                backoffSec = 5
                jitter(agent.prefs.heartbeatSeconds.toLong())
            } catch (e: InterruptedException) {
                return
            } catch (e: ApiException) {
                Log.w(Agent.TAG, "check-in rejected: ${e.status} ${e.message}")
                if (e.status == 401) Log.e(Agent.TAG, "server does not know this device; it may have been deleted. Factory-reset and re-provision to enroll again.")
                backoffSec.also { backoffSec = min(backoffSec * 2, 300) }
            } catch (e: Exception) {
                Log.w(Agent.TAG, "check-in failed: $e")
                backoffSec.also { backoffSec = min(backoffSec * 2, 300) }
            }
            try {
                Thread.sleep(wait * 1000)
            } catch (e: InterruptedException) {
                return
            }
        }
    }

    private fun jitter(sec: Long): Long = (sec * (0.9 + Math.random() * 0.2)).toLong().coerceAtLeast(5)

    private fun startInForeground() {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL, getString(R.string.channel_name), NotificationManager.IMPORTANCE_MIN))
        val n = Notification.Builder(this, CHANNEL)
            .setSmallIcon(android.R.drawable.ic_lock_lock)
            .setContentTitle(getString(R.string.app_name))
            .setContentText(getString(R.string.notification_text))
            .setOngoing(true)
            .build()
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIFICATION_ID, n)
        }
    }

    companion object {
        private const val CHANNEL = "armada"
        private const val NOTIFICATION_ID = 1

        fun start(context: Context) {
            context.startForegroundService(Intent(context, AgentService::class.java))
        }

        fun restart(context: Context) {
            context.stopService(Intent(context, AgentService::class.java))
            start(context)
        }
    }
}
