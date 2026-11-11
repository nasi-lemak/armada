package dev.armada.client

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File
import java.math.BigInteger
import java.security.KeyPairGenerator
import java.security.MessageDigest
import java.security.Signature
import java.security.interfaces.ECPublicKey
import java.security.spec.ECGenParameterSpec
import java.util.Base64

class DpopTest {
    private val dec = Base64.getUrlDecoder()

    @Test
    fun proofIsAValidES256Jws() {
        val kp = KeyPairGenerator.getInstance("EC").apply { initialize(ECGenParameterSpec("secp256r1")) }.generateKeyPair()
        val jwk = Dpop.jwk(kp.public as ECPublicKey)
        val token = "header.payload.signature"
        val proof = Dpop.proof("post", "https://api.example.com/v1/orders?page=2#x", token, jwk, 1_700_000_000, "jti-1") { input ->
            Signature.getInstance("SHA256withECDSA").run { initSign(kp.private); update(input); sign() }
        }
        val parts = proof.split(".")
        assertEquals(3, parts.size)
        val header = JSONObject(String(dec.decode(parts[0])))
        val payload = JSONObject(String(dec.decode(parts[1])))
        assertEquals("dpop+jwt", header.getString("typ"))
        assertEquals("ES256", header.getString("alg"))
        assertEquals(jwk.getString("x"), header.getJSONObject("jwk").getString("x"))
        assertEquals("POST", payload.getString("htm"))
        assertEquals("https://api.example.com/v1/orders", payload.getString("htu"))
        val ath = Base64.getUrlEncoder().withoutPadding().encodeToString(MessageDigest.getInstance("SHA-256").digest(token.toByteArray()))
        assertEquals(ath, payload.getString("ath"))

        // r||s signature must verify once converted back to DER.
        val raw = dec.decode(parts[2])
        assertEquals(64, raw.size)
        val ok = Signature.getInstance("SHA256withECDSA").run {
            initVerify(kp.public)
            update((parts[0] + "." + parts[1]).toByteArray())
            verify(rawToDer(raw))
        }
        assertTrue("signature does not verify", ok)

        // Hand the proof to the Go verifier in an interop check (see README).
        System.getProperty("dpop.out")?.let { path ->
            File(path).writeText(JSONObject().put("token", token).put("proof", proof).put("jwk", jwk).toString())
        }
    }

    @Test
    fun derToRawPadsShortIntegers() {
        // r = 1, s = 0x80 (needs a leading zero in DER).
        val der = byteArrayOf(0x30, 0x07, 0x02, 0x01, 0x01, 0x02, 0x02, 0x00, 0x80.toByte())
        val raw = Dpop.derToRaw(der)
        assertEquals(64, raw.size)
        assertEquals(BigInteger.ONE, BigInteger(1, raw.copyOfRange(0, 32)))
        assertEquals(BigInteger.valueOf(0x80), BigInteger(1, raw.copyOfRange(32, 64)))
    }

    private fun rawToDer(raw: ByteArray): ByteArray {
        fun int(b: ByteArray): ByteArray {
            val v = BigInteger(1, b).toByteArray()
            return byteArrayOf(0x02, v.size.toByte()) + v
        }
        val body = int(raw.copyOfRange(0, 32)) + int(raw.copyOfRange(32, 64))
        return byteArrayOf(0x30, body.size.toByte()) + body
    }
}
