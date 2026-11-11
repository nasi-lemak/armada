package dev.armada.agent

import android.app.ActivityManager
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.os.BatteryManager
import android.os.PowerManager
import android.os.StatFs
import org.json.JSONArray
import org.json.JSONObject
import java.time.Instant

/**
 * Health checks Android exposes to a device-owner app: battery (charge,
 * health, temperature), device temperature (thermal status), storage space
 * and memory. Android does not let apps read storage wear, so flash health
 * is not available here.
 */
object Health {
    fun checks(context: Context): JSONArray {
        val now = Instant.now().toString()
        val out = JSONArray()
        fun add(name: String, kind: String, status: String, message: String, value: Double? = null, unit: String? = null, details: Map<String, String> = emptyMap()) {
            val o = JSONObject().put("name", name).put("kind", kind).put("status", status).put("message", message).put("checked_at", now)
            if (value != null) o.put("value", value)
            if (unit != null) o.put("unit", unit)
            if (details.isNotEmpty()) o.put("details", JSONObject(details))
            out.put(o)
        }

        context.registerReceiver(null, IntentFilter(Intent.ACTION_BATTERY_CHANGED))?.let { b ->
            val level = b.getIntExtra(BatteryManager.EXTRA_LEVEL, -1)
            val scale = b.getIntExtra(BatteryManager.EXTRA_SCALE, 100)
            val pct = if (level >= 0 && scale > 0) level * 100.0 / scale else -1.0
            val tempC = b.getIntExtra(BatteryManager.EXTRA_TEMPERATURE, Int.MIN_VALUE).let { if (it == Int.MIN_VALUE) null else it / 10.0 }
            val plugged = b.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0) != 0
            val (status, msg) = when (b.getIntExtra(BatteryManager.EXTRA_HEALTH, BatteryManager.BATTERY_HEALTH_UNKNOWN)) {
                BatteryManager.BATTERY_HEALTH_DEAD -> "critical" to "battery is dead"
                BatteryManager.BATTERY_HEALTH_OVERHEAT -> "critical" to "battery is overheating"
                BatteryManager.BATTERY_HEALTH_OVER_VOLTAGE -> "critical" to "battery over-voltage"
                BatteryManager.BATTERY_HEALTH_UNSPECIFIED_FAILURE -> "critical" to "battery failure"
                BatteryManager.BATTERY_HEALTH_COLD -> "warning" to "battery is too cold"
                else -> when {
                    pct in 0.0..9.99 && !plugged -> "warning" to "battery low (${pct.toInt()}%)"
                    tempC != null && tempC >= 50 -> "warning" to "battery hot (${tempC}°C)"
                    pct >= 0 -> "ok" to "battery ${pct.toInt()}%${if (plugged) ", charging" else ""}"
                    else -> "unknown" to "battery state unknown"
                }
            }
            add("battery", "battery", status, msg, if (pct >= 0) pct else null, "%",
                buildMap { tempC?.let { put("temperature_c", it.toString()) }; put("plugged", plugged.toString()) })
        }

        val pm = context.getSystemService(PowerManager::class.java)
        val (tStatus, tMsg) = when (pm.currentThermalStatus) {
            PowerManager.THERMAL_STATUS_NONE, PowerManager.THERMAL_STATUS_LIGHT -> "ok" to "temperature normal"
            PowerManager.THERMAL_STATUS_MODERATE -> "warning" to "device is warm; performance may be limited"
            PowerManager.THERMAL_STATUS_SEVERE -> "warning" to "device is hot; performance is throttled"
            PowerManager.THERMAL_STATUS_CRITICAL, PowerManager.THERMAL_STATUS_EMERGENCY, PowerManager.THERMAL_STATUS_SHUTDOWN ->
                "critical" to "device is overheating"
            else -> "unknown" to "thermal status unavailable"
        }
        add("temperature device", "temperature", tStatus, tMsg)

        val fs = StatFs(context.filesDir.absolutePath)
        if (fs.totalBytes > 0) {
            val used = 100.0 * (fs.totalBytes - fs.availableBytes) / fs.totalBytes
            val st = when {
                used >= 95 -> "critical"
                used >= 85 -> "warning"
                else -> "ok"
            }
            add("disk-space /data", "disk_space", st, "storage is ${used.toInt()}% full", Math.round(used * 10) / 10.0, "%",
                mapOf("free_gb" to "%.1f".format(fs.availableBytes / 1e9)))
        }

        val mem = ActivityManager.MemoryInfo().also { context.getSystemService(ActivityManager::class.java).getMemoryInfo(it) }
        if (mem.totalMem > 0) {
            val avail = 100.0 * mem.availMem / mem.totalMem
            val st = if (mem.lowMemory) "warning" else "ok"
            add("memory", "memory", st, "${avail.toInt()}% of memory available" + if (mem.lowMemory) " (system reports low memory)" else "",
                Math.round(avail * 10) / 10.0, "% available")
        }
        return out
    }
}
