plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    // Change applicationId to your own namespace (e.g. com.yourco.fleet.agent)
    // and pass the same value to armada-server -android-agent-package.
    namespace = "dev.armada.agent"
    compileSdk = 35
    buildToolsVersion = "35.0.0"

    defaultConfig {
        applicationId = "dev.armada.agent"
        minSdk = 29
        targetSdk = 35
        versionCode = 1
        versionName = "1.0.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    buildFeatures {
        buildConfig = true
    }
}

dependencies {
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.json:json:20240303")
    // Ed25519 verification of release signatures (not in the platform before Android 13).
    implementation("com.google.crypto.tink:tink-android:1.15.0")
}
