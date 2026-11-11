package dev.armada.agent

import org.json.JSONObject
import java.io.File
import java.io.FileOutputStream
import java.net.HttpURLConnection
import java.net.URL

class ApiException(val status: Int, message: String) : Exception(message)

/** Minimal JSON-over-HTTPS client for the Armada agent protocol. */
class ApiClient(private val baseUrl: String, private val deviceKey: () -> String?) {

    fun post(path: String, body: JSONObject?, auth: Boolean = true): JSONObject {
        val conn = open(path, auth)
        conn.requestMethod = "POST"
        conn.doOutput = true
        conn.setRequestProperty("Content-Type", "application/json")
        conn.outputStream.use { it.write((body ?: JSONObject()).toString().toByteArray()) }
        return readJson(conn)
    }

    /** GET with a custom read timeout, for the push long-poll. */
    fun get(path: String, readTimeoutMs: Int = 30_000): JSONObject {
        val conn = open(path, true)
        conn.readTimeout = readTimeoutMs
        return readJson(conn)
    }

    /**
     * Downloads [path] into [dest], resuming if [dest] already holds part of
     * the file. Large APKs on flaky mobile links benefit from this.
     */
    fun download(path: String, dest: File, expectedSize: Long) {
        val have = if (dest.exists()) dest.length() else 0L
        if (expectedSize > 0 && have == expectedSize) return
        val resumeFrom = if (expectedSize in 1 until have) 0L else have
        val conn = open(path, true)
        conn.readTimeout = 120_000
        if (resumeFrom > 0) conn.setRequestProperty("Range", "bytes=$resumeFrom-")
        val append = when (conn.responseCode) {
            HttpURLConnection.HTTP_PARTIAL -> true
            HttpURLConnection.HTTP_OK -> false
            else -> throw error(conn)
        }
        conn.inputStream.use { input ->
            FileOutputStream(dest, append).use { out -> input.copyTo(out, 64 * 1024) }
        }
    }

    private fun open(path: String, auth: Boolean): HttpURLConnection {
        val conn = URL(baseUrl + path).openConnection() as HttpURLConnection
        conn.connectTimeout = 20_000
        conn.readTimeout = 30_000
        conn.setRequestProperty("User-Agent", "armada-android-agent/${BuildConfig.VERSION_NAME}")
        if (auth) deviceKey()?.let { conn.setRequestProperty("Authorization", "Bearer $it") }
        return conn
    }

    private fun readJson(conn: HttpURLConnection): JSONObject {
        val code = conn.responseCode
        if (code >= 300) throw error(conn)
        if (code == HttpURLConnection.HTTP_NO_CONTENT) return JSONObject()
        val text = conn.inputStream.use { it.readBytes().toString(Charsets.UTF_8) }
        return if (text.isBlank()) JSONObject() else JSONObject(text)
    }

    private fun error(conn: HttpURLConnection): ApiException {
        val text = (conn.errorStream ?: return ApiException(conn.responseCode, "HTTP ${conn.responseCode}"))
            .use { it.readBytes().toString(Charsets.UTF_8) }
        val msg = runCatching { JSONObject(text).optString("error") }.getOrNull().orEmpty().ifEmpty { text.take(300) }
        return ApiException(conn.responseCode, msg)
    }
}
