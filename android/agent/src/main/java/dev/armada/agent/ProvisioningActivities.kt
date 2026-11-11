package dev.armada.agent

import android.app.Activity
import android.app.admin.DevicePolicyManager
import android.content.Intent
import android.os.Bundle

/**
 * Android 10+ asks the device policy app which mode to provision. Armada
 * manages company-provided devices, so it always asks for a fully managed
 * device (Device Owner).
 */
class ProvisioningModeActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        AdminReceiver.saveProvisioningExtras(this, intent)
        val result = Intent().putExtra(DevicePolicyManager.EXTRA_PROVISIONING_MODE, DevicePolicyManager.PROVISIONING_MODE_FULLY_MANAGED_DEVICE)
        setResult(RESULT_OK, result)
        finish()
    }
}

/** Runs at the end of setup: apply policy and start checking in. */
class PolicyComplianceActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        AdminReceiver.saveProvisioningExtras(this, intent)
        AgentService.start(this)
        setResult(RESULT_OK)
        finish()
    }
}
