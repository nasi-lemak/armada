package dev.armada.agent

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.Socket
import java.net.URL
import java.security.cert.X509Certificate
import javax.net.ssl.HttpsURLConnection

/**
 * The on-demand network check: the device's route to the server, step by
 * step (network, proxy, DNS, TCP, TLS, HTTPS), reported as text. The
 * second value is false at the first step that fails.
 */
object NetCheck {
    fun run(context: Context, serverUrl: String): Pair<String, Boolean> {
        val b = StringBuilder()
        fun line(label: String, text: String) = b.append(label.padEnd(11)).append(' ').append(text).append('\n')
        fun ms(t0: Long) = "${(System.nanoTime() - t0) / 1_000_000} ms"
        b.append("Network check from ${android.os.Build.MODEL} at ${isoTime(System.currentTimeMillis())}\n\n")
        val url = try { URL(serverUrl) } catch (e: Exception) {
            line("Server", "invalid server URL $serverUrl"); return b.toString() to false
        }
        line("Server", serverUrl)
        val port = if (url.port > 0) url.port else url.defaultPort

        // The active network: what it is, whether Android thinks it reaches the internet.
        val cm = context.getSystemService(ConnectivityManager::class.java)
        val net = cm.activeNetwork
        val caps = net?.let { cm.getNetworkCapabilities(it) }
        val link = net?.let { cm.getLinkProperties(it) }
        if (net == null || caps == null) {
            line("Network", "FAILED: no active network")
            return b.append("\nResult: the device has no network connection.\n").toString() to false
        }
        val kind = when {
            caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) -> "ethernet"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) -> "Wi-Fi"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) -> "mobile data"
            caps.hasTransport(NetworkCapabilities.TRANSPORT_VPN) -> "VPN"
            else -> "other"
        }
        val validated = caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        line("Network", "$kind · ${if (validated) "internet validated" else "no validated internet (fine on a closed network)"}")
        link?.let { lp ->
            val addrs = lp.linkAddresses.joinToString(" ") { it.toString() }
            if (addrs.isNotEmpty()) line("Interfaces", "${lp.interfaceName ?: "?"} $addrs")
            lp.routes.firstOrNull { it.isDefaultRoute && it.gateway != null }?.let { line("Gateway", it.gateway!!.hostAddress ?: "?") }
            if (lp.dnsServers.isNotEmpty()) line("DNS servers", lp.dnsServers.joinToString(", ") { it.hostAddress ?: "?" })
        }
        val proxy = link?.httpProxy
        if (proxy != null && proxy.host != null) line("Proxy", "${proxy.host}:${proxy.port}") else line("Proxy", "none: connecting directly")

        // DNS.
        var t0 = System.nanoTime()
        val ips = try {
            InetAddress.getAllByName(url.host).toList()
        } catch (e: Exception) {
            line("DNS", "FAILED: ${url.host} (${e.message ?: e.javaClass.simpleName})")
            if (proxy == null) return b.append("\nResult: the device can't resolve the server's name. Check its DNS settings.\n").toString() to false
            emptyList()
        }
        if (ips.isNotEmpty()) line("DNS", "${url.host} → ${ips.take(4).joinToString(", ") { it.hostAddress ?: "?" }} (${ms(t0)})")

        // TCP to the server (or the proxy).
        val (tcpHost, tcpPort, what) = if (proxy?.host != null) Triple(proxy.host, proxy.port, "proxy") else Triple(url.host, port, "server")
        t0 = System.nanoTime()
        try {
            Socket().use { it.connect(InetSocketAddress(tcpHost, tcpPort), 10_000) }
            line("TCP", "$what $tcpHost:$tcpPort connected in ${ms(t0)}")
        } catch (e: Exception) {
            line("TCP", "FAILED: $what $tcpHost:$tcpPort (${e.message ?: e.javaClass.simpleName})")
            return b.append("\nResult: the device can't open a connection to the $what.\n").toString() to false
        }

        // HTTPS round trips (TLS details from the first).
        val times = mutableListOf<Long>()
        var status = 0
        var lastError = ""
        for (i in 0 until 3) {
            t0 = System.nanoTime()
            try {
                val c = URL(serverUrl.trimEnd('/') + "/healthz").openConnection() as java.net.HttpURLConnection
                c.connectTimeout = 10_000; c.readTimeout = 10_000
                status = c.responseCode
                c.inputStream.use { it.readBytes() }
                if (i == 0 && c is HttpsURLConnection) {
                    val cert = c.serverCertificates.firstOrNull() as? X509Certificate
                    val days = cert?.let { (it.notAfter.time - System.currentTimeMillis()) / 86_400_000 }
                    line("TLS", "${c.cipherSuite}${cert?.let { " · ${it.subjectX500Principal.name} · expires in $days days · trusted" } ?: ""}")
                }
                c.disconnect()
                times.add((System.nanoTime() - t0) / 1_000_000)
            } catch (e: javax.net.ssl.SSLException) {
                line("TLS", "FAILED: ${e.message ?: e.javaClass.simpleName}")
                return b.append("\nResult: the TLS handshake failed; the device may not trust the server's certificate.\n").toString() to false
            } catch (e: Exception) {
                lastError = e.message ?: e.javaClass.simpleName
            }
        }
        if (times.isEmpty()) {
            line(url.protocol.uppercase(), "FAILED: $lastError")
            return b.append("\nResult: the connection opens but requests to the server fail.\n").toString() to false
        }
        times.sort()
        line(url.protocol.uppercase(), "/healthz $status in ${times[times.size / 2]} ms (${times.size} of 3 tries: ${times.joinToString(", ") { "$it ms" }})")
        return when {
            status != 200 -> b.append("\nResult: the server answered $status. A proxy or load balancer may be misrouting.\n").toString() to false
            times.size < 3 -> b.append("\nResult: the server is reachable, but some requests failed: the connection is unreliable.\n").toString() to true
            else -> b.append("\nResult: OK. The server is reachable.\n").toString() to true
        }
    }
}
