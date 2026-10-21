import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
    id("org.jetbrains.kotlin.plugin.serialization")
}

// R.jar keeps getting stuck under an OS-level lock whenever resources change
// -- VS Code's Java/Gradle tooling (vscode-gradle's build server, and the
// Java language server that imports this project) holds the build output
// open and respawns if killed. Each time, pointing the build at a fresh
// directory has been the way past it; the old ones (build3, ...) can be
// deleted once the lock lets go. Excluding androidtv/ from VS Code's Java
// import would stop it for good.
layout.buildDirectory.set(layout.projectDirectory.dir("build5"))

// Release signing lives outside version control (see .gitignore) -- a fresh
// clone with no keystore.properties still builds debug fine; only
// assembleRelease needs it, and fails with a clear message below if it's
// missing rather than silently producing an unsigned/unimportable APK.
val keystorePropsFile = rootProject.file("keystore.properties")
val keystoreProps = Properties().apply {
    if (keystorePropsFile.exists()) keystorePropsFile.inputStream().use { load(it) }
}

android {
    namespace = "com.venfriti.stream"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.venfriti.stream"
        // Real Android TV boxes (Chromecast with Google TV, Nvidia Shield,
        // Fire TV running stock Android TV OS) all run well above this --
        // set low enough to also cover older set-top hardware rather than
        // chasing the latest API level for its own sake.
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "1.0.0"

        // The server's fixed address. Used until Settings overrides it; change
        // it here (or pass -PstreamServer=http://host:port) for another box.
        val server = (project.findProperty("streamServer") as String?) ?: "http://192.168.1.159:5746"
        buildConfigField("String", "DEFAULT_SERVER_URL", "\"$server\"")
    }

    signingConfigs {
        create("release") {
            if (keystorePropsFile.exists()) {
                storeFile = rootProject.file(keystoreProps.getProperty("storeFile"))
                storePassword = keystoreProps.getProperty("storePassword")
                keyAlias = keystoreProps.getProperty("keyAlias")
                keyPassword = keystoreProps.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            if (keystorePropsFile.exists()) {
                signingConfig = signingConfigs.getByName("release")
            }
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
        compose = true
        // Exposes BuildConfig.VERSION_NAME, compared against the server's
        // reported latest version to decide whether to show the update
        // banner (see UpdateManager.kt).
        buildConfig = true
    }

    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
    }
}

dependencies {
    val composeBom = platform("androidx.compose:compose-bom:2024.12.01")
    implementation(composeBom)

    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.7")
    // LocalLifecycleOwner for the player's pause-and-save on ON_STOP.
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.8.7")

    // Compose for TV: D-pad/focus-aware components (Card, Carousel, lazy
    // rows that auto-scroll a focused item into view) that plain Compose
    // Material3 doesn't provide -- TV has no touch/hover, only focus.
    implementation("androidx.tv:tv-foundation:1.0.0-alpha12")
    implementation("androidx.tv:tv-material:1.0.1")

    // Media3/ExoPlayer for playback (HLS + direct-play), with its own
    // Compose-friendly PlayerView wrapper.
    implementation("androidx.media3:media3-exoplayer:1.5.1")
    implementation("androidx.media3:media3-exoplayer-hls:1.5.1")
    implementation("androidx.media3:media3-ui:1.5.1")
    implementation("androidx.media3:media3-session:1.5.1")

    // OkHttp for the session-cookie-carrying HTTP client (mirrors
    // HttpClient+CookieContainer on the Windows client) and coil for
    // poster/thumbnail image loading in Compose.
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    implementation("io.coil-kt:coil-compose:2.7.0")
    // Ambient background: samples a poster/backdrop down to an accent colour
    // (Palette) instead of ever painting the raw photo full-screen behind
    // the wall -- see TintBackdrop in Rails.kt.
    implementation("androidx.palette:palette-ktx:1.0.0")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.9.0")
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3")
}
