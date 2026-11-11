package dev.armada.client

import android.content.Context
import android.net.Uri
import android.os.Bundle
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import org.json.JSONObject
import java.math.BigInteger
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.MessageDigest
import java.security.PrivateKey
import java.security.Signature
import java.security.interfaces.ECPublicKey
import java.security.spec.ECGenParameterSpec
import java.util.UUID

/** Thrown when no license can be obtained (device not enrolled, revoked, app not registered, offline…). */
class LicenseException(message: String, val status: Int = 0) : Exception(message)

/**
 * Gets this app's Armada license from the device agent and signs requests to
 * your backend with it.
 *
 * ```
 * val armada = ArmadaLicense(context)
 * // On a background thread, for every request to your backend:
 * val headers = armada.authHeaders("POST", "https://api.example.com/v1/orders")
 * headers.forEach { (k, v) -> connection.setRequestProperty(k, v) }
 * ```
 *
 * Each request carries the license (`Authorization: DPoP <license>`) and a
 * DPoP proof (`DPoP: <proof>`) signed by a key kept in this app's hardware
 * Keystore. The license is bound to that key, so a copied license is useless
 * without the device it was issued to. Verify both on your backend with
 * Armada's `pkg/license` package or any JWT library.
 *
 * On a device that was not provisioned by you (for example, the APK was
 * copied to another phone), there is no enrolled agent and [license] throws.
 */
class ArmadaLicense(context: Context) {
    private val resolver = context.applicationContext.contentResolver

    @Volatile private var cached: Pair<String, Long>? = null

    /** Returns a valid license, fetching a new one shortly before the current one expires. Blocking. */
    @Synchronized
    fun license(forceRefresh: Boolean = false): String {
        cached?.let { (token, exp) ->
            if (!forceRefresh && exp - System.currentTimeMillis() > 2 * 60_000) return token
        }
        val extras = Bundle().apply {
            putString("app_key_jwk", publicJwk().toString())
            putBoolean("refresh", forceRefresh)
        }
        val result = try {
            resolver.call(AUTHORITY, "getLicense", null, extras)
        } catch (e: Exception) {
            null
        } ?: throw LicenseException("the Armada agent is not installed on this device")
        result.getString("error")?.let { throw LicenseException(it, result.getInt("status")) }
        val token = result.getString("license") ?: throw LicenseException("agent returned no license")
        cached = token to result.getLong("expires_at")
        return token
    }

    /** Headers for one request: `Authorization` and `DPoP`. Blocking. */
    fun authHeaders(method: String, url: String): Map<String, String> {
        val token = license()
        return mapOf("Authorization" to "DPoP $token", "DPoP" to dpopProof(method, url, token))
    }

    /** Creates an RFC 9449 DPoP proof for one request. */
    fun dpopProof(method: String, url: String, token: String): String {
        val key = privateKey()
        return Dpop.proof(method, url, token, publicJwk(), System.currentTimeMillis() / 1000, UUID.randomUUID().toString()) { input ->
            Signature.getInstance("SHA256withECDSA").run {
                initSign(key)
                update(input)
                sign()
            }
        }
    }

    // ---- the app's DPoP key, held in the hardware Keystore ----

    private fun keyStore() = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }

    private fun privateKey(): PrivateKey {
        val ks = keyStore()
        if (!ks.containsAlias(ALIAS)) {
            KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore").apply {
                initialize(
                    KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
                        .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
                        .setDigests(KeyProperties.DIGEST_SHA256)
                        .build()
                )
                generateKeyPair()
            }
        }
        return ks.getKey(ALIAS, null) as PrivateKey
    }

    private fun publicJwk(): JSONObject {
        privateKey() // ensure it exists
        return Dpop.jwk(keyStore().getCertificate(ALIAS).publicKey as ECPublicKey)
    }

    companion object {
        private val AUTHORITY: Uri = Uri.parse("content://dev.armada.agent.license")
        private const val ALIAS = "armada_dpop"
    }
}

/** Pure DPoP helpers (no Android dependencies, so they are unit-tested on the JVM). */
internal object Dpop {
    private val enc = java.util.Base64.getUrlEncoder().withoutPadding()

    fun b64(b: ByteArray): String = enc.encodeToString(b)

    fun jwk(pub: ECPublicKey): JSONObject = JSONObject()
        .put("kty", "EC")
        .put("crv", "P-256")
        .put("x", b64(unsigned32(pub.w.affineX)))
        .put("y", b64(unsigned32(pub.w.affineY)))

    /** Builds a DPoP proof; [signDer] returns a DER SHA256withECDSA signature of its input. */
    fun proof(method: String, url: String, token: String, jwk: JSONObject, nowSec: Long, jti: String, signDer: (ByteArray) -> ByteArray): String {
        val header = JSONObject().put("typ", "dpop+jwt").put("alg", "ES256").put("jwk", jwk)
        val ath = b64(MessageDigest.getInstance("SHA-256").digest(token.toByteArray(Charsets.US_ASCII)))
        val payload = JSONObject()
            .put("jti", jti)
            .put("htm", method.uppercase())
            .put("htu", url.substringBefore('?').substringBefore('#'))
            .put("iat", nowSec)
            .put("ath", ath)
        val input = b64(header.toString().toByteArray()) + "." + b64(payload.toString().toByteArray())
        return input + "." + b64(derToRaw(signDer(input.toByteArray(Charsets.US_ASCII))))
    }

    fun unsigned32(v: BigInteger): ByteArray {
        val b = v.toByteArray()
        return when {
            b.size == 32 -> b
            b.size > 32 -> b.copyOfRange(b.size - 32, b.size)
            else -> ByteArray(32 - b.size) + b
        }
    }

    /** Converts a DER ECDSA signature to the 64-byte r||s form JWS uses. */
    fun derToRaw(der: ByteArray): ByteArray {
        var i = 2
        if (der[1].toInt() and 0x80 != 0) i += der[1].toInt() and 0x7f
        fun readInt(): BigInteger {
            require(der[i].toInt() == 0x02) { "bad DER signature" }
            val len = der[i + 1].toInt()
            val v = BigInteger(1, der.copyOfRange(i + 2, i + 2 + len))
            i += 2 + len
            return v
        }
        val r = readInt()
        val s = readInt()
        return unsigned32(r) + unsigned32(s)
    }
}
