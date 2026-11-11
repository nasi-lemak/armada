package dev.armada.agent

import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Test

class ThumbprintTest {
    @Test
    fun matchesServerAndPublishedVector() {
        // Same key and expected thumbprint as the Go test in pkg/license.
        val jwk = JSONObject()
            .put("kty", "EC").put("crv", "P-256")
            .put("x", "f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU")
            .put("y", "x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0")
            .put("kid", "ignored")
        assertEquals("oKIywvGUpTVTyxMQ3bwIIeQUudfr_CkLMjCE19ECD-U", Agent.thumbprint(jwk))
    }

    @Test
    fun goArchMapping() {
        assertEquals("arm64", Agent.goArch("arm64-v8a"))
        assertEquals("arm", Agent.goArch("armeabi-v7a"))
        assertEquals("amd64", Agent.goArch("x86_64"))
    }
}
