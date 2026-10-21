package com.venfriti.stream.data

import android.content.Context
import com.venfriti.stream.BuildConfig

// Where the app looks for the server. The address is fixed, so it's built in
// (BuildConfig.DEFAULT_SERVER_URL, set in app/build.gradle.kts) and only
// stored here when changed in Settings.
//
// lastGoodUrl is what makes a failure readable: an address that has worked
// before and now doesn't answer means "the server is down", so the app waits
// and retries; one that has never worked means "wrong address", so it asks.
class ServerSettings(context: Context) {
    private val prefs = context.getSharedPreferences("server", Context.MODE_PRIVATE)

    val defaultUrl: String get() = BuildConfig.DEFAULT_SERVER_URL

    var serverUrl: String
        get() = prefs.getString("url", null) ?: defaultUrl
        set(value) {
            val v = normaliseUrl(value)
            prefs.edit().apply { if (v == defaultUrl) remove("url") else putString("url", v) }.apply()
        }

    var lastGoodUrl: String?
        get() = prefs.getString("last_good", null)
        set(value) = prefs.edit().putString("last_good", value).apply()

    var lastConnectedAt: Long
        get() = prefs.getLong("last_connected_at", 0)
        set(value) = prefs.edit().putLong("last_connected_at", value).apply()

    // Optional: the server's MAC address, for the "Wake server" button.
    var wakeMac: String
        get() = prefs.getString("wake_mac", "") ?: ""
        set(value) = prefs.edit().putString("wake_mac", value.trim()).apply()

    companion object {
        // "192.168.1.159:5746" -> "http://192.168.1.159:5746" -- typing the
        // scheme on a remote is exactly the fiddliness to avoid.
        fun normaliseUrl(raw: String): String {
            var u = raw.trim().trimEnd('/')
            if (u.isEmpty()) return u
            if (!u.startsWith("http://", ignoreCase = true) && !u.startsWith("https://", ignoreCase = true)) u = "http://$u"
            return u
        }
    }
}
