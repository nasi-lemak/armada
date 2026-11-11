package dev.armada.agent

import android.content.pm.PackageManager
import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.security.keystore.StrongBoxUnavailableException
import android.util.Base64
import android.util.Log
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec

/**
 * The device identity key. It is generated inside the secure hardware
 * (StrongBox if present, otherwise the TEE), can never be exported, and comes
 * with an attestation certificate chain signed by Google that proves both.
 * Licenses are only issued to requests signed by this key, so copying the
 * agent's data to another phone gets nothing.
 */
object HardwareKey {
    private const val ALIAS = "armada_device_key"
    private const val TAG = "ArmadaKey"

    /** Creates a fresh key bound to [challenge] and returns its attestation chain (base64 DER, leaf first). */
    fun generate(context: Context, challenge: ByteArray): List<String> {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        if (ks.containsAlias(ALIAS)) ks.deleteEntry(ALIAS)

        val strongBox = context.packageManager.hasSystemFeature(PackageManager.FEATURE_STRONGBOX_KEYSTORE)
        try {
            create(challenge, strongBox)
        } catch (e: StrongBoxUnavailableException) {
            Log.i(TAG, "StrongBox unavailable, using TEE")
            create(challenge, false)
        }
        val chain = ks.getCertificateChain(ALIAS) ?: error("Keystore returned no attestation chain")
        return chain.map { Base64.encodeToString(it.encoded, Base64.NO_WRAP) }
    }

    private fun create(challenge: ByteArray, strongBox: Boolean) {
        val spec = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .setAttestationChallenge(challenge)
            .setIsStrongBoxBacked(strongBox)
            .build()
        KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore").apply {
            initialize(spec)
            generateKeyPair()
        }
    }

    /** Signs [data] with SHA256withECDSA (DER-encoded signature). */
    fun sign(data: ByteArray): ByteArray {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        val key = ks.getKey(ALIAS, null) as? PrivateKey ?: error("device key missing; the device must re-enroll")
        return Signature.getInstance("SHA256withECDSA").run {
            initSign(key)
            update(data)
            sign()
        }
    }
}
