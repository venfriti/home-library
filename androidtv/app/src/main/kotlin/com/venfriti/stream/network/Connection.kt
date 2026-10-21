package com.venfriti.stream.network

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import com.venfriti.stream.data.Library
import com.venfriti.stream.data.ServerSettings
import java.io.File
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.withTimeoutOrNull

sealed interface ConnState {
    // First check on launch, nothing known yet.
    data object Checking : ConnState
    data object Online : ConnState
    // An address that has worked before isn't answering: the server is off,
    // asleep or starting. Keep retrying; never ask to retype a fixed address.
    data class Reconnecting(val attempt: Int, val nextRetryAtMs: Long) : ConnState
    // Never worked, or answered like something else: ask.
    data class NeedsAddress(val url: String, val reason: String) : ConnState
    // The TV itself has no network -- not the server's fault.
    data object DeviceOffline : ConnState
}

// Owns "which server, is it there, and what to show meanwhile". Runs one loop
// for the life of the app: probe, then either settle into a slow heartbeat
// (online) or a backing-off retry (down). Anything that should re-check
// straight away -- the TV's network coming back, the app returning to the
// foreground, a button press -- just pokes the loop.
private const val LibraryRefreshMs = 5 * 60_000L

class ConnectionController(context: Context, val settings: ServerSettings) {
    private val appContext = context.applicationContext
    private val cacheFile = File(appContext.filesDir, "library.json")
    private val poke = Channel<Unit>(Channel.CONFLATED)

    var state by mutableStateOf<ConnState>(ConnState.Checking)
        private set
    var client by mutableStateOf<ServerClient?>(null)
        private set
    var library by mutableStateOf<Library?>(null)
        private set

    private var attempt = 0

    // Library reloads while the app stays open: the server walks its folders
    // fresh on every request, so this is what makes newly added files (and
    // posters/titles that arrived since) appear without restarting the app.
    // Runs on request (back to the app, "Refresh library"), every
    // LibraryRefreshMs while online, and when the server comes back -- but
    // not during playback (`refreshPaused`, set by the UI).
    private var lastLibraryLoadAt = 0L
    private var reloadRequested = false
    var refreshPaused = false
    var refreshing by mutableStateOf(false)
        private set
    var lastRefreshedAt by mutableStateOf(0L)
        private set

    fun requestLibraryRefresh() {
        reloadRequested = true
        retryNow()
    }

    private suspend fun reloadLibrary() {
        reloadRequested = false
        val lib = library ?: return
        refreshing = true
        try {
            lib.load()
            lastLibraryLoadAt = System.currentTimeMillis()
            lastRefreshedAt = lastLibraryLoadAt
        } catch (e: Exception) {
            // Keep what's on screen; the next heartbeat tries again.
        } finally {
            refreshing = false
        }
    }

    // "Change server" pressed on the reconnect screen: hold the form up
    // instead of letting the next retry replace it.
    private var manualSetup = false
    val inManualSetup: Boolean get() = manualSetup

    fun useAddressPrompt() {
        manualSetup = true
        state = ConnState.NeedsAddress(settings.serverUrl, "Change the address below, or keep waiting for this one.")
        poke.trySend(Unit)
    }

    fun cancelPrompt() {
        manualSetup = false
        retryNow()
    }

    fun retryNow() {
        attempt = 0
        poke.trySend(Unit)
    }

    // Settings / the setup screen. A changed address starts from scratch:
    // new client, new library, and it has to prove itself before counting as
    // "last good".
    fun useAddress(url: String) {
        manualSetup = false
        val normalised = ServerSettings.normaliseUrl(url)
        if (normalised != client?.address) {
            client = null
            library = null
        }
        settings.serverUrl = normalised
        state = ConnState.Checking
        retryNow()
    }

    suspend fun wakeServer(): Boolean {
        val sent = sendWakeOnLan(settings.wakeMac)
        // A waking PC takes a while; check often for the next couple of minutes.
        attempt = 0
        poke.trySend(Unit)
        return sent
    }

    private fun networkAvailable(): Boolean {
        val cm = appContext.getSystemService(ConnectivityManager::class.java) ?: return true
        val caps = cm.getNetworkCapabilities(cm.activeNetwork) ?: return false
        return caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) ||
            caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) ||
            caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)
    }

    // Network up/down on the TV -> re-check immediately rather than waiting
    // out the current retry delay. Returns an unregister function.
    fun watchNetwork(): () -> Unit {
        val cm = appContext.getSystemService(ConnectivityManager::class.java) ?: return {}
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) { retryNow() }
            override fun onLost(network: Network) { poke.trySend(Unit) }
        }
        return try {
            cm.registerDefaultNetworkCallback(cb)
            val unregister: () -> Unit = { runCatching { cm.unregisterNetworkCallback(cb) } }
            unregister
        } catch (e: Exception) {
            {}
        }
    }

    private suspend fun waitForPoke(timeoutMs: Long?) {
        if (timeoutMs == null) poke.receive() else withTimeoutOrNull(timeoutMs) { poke.receive() }
    }

    private fun backoffMs(n: Int): Long = when (n) {
        0, 1 -> 3_000
        2 -> 5_000
        3 -> 10_000
        4 -> 20_000
        else -> 30_000
    }

    // With a cached copy on the TV, a down server still gets you the wall
    // (with an offline banner) instead of a blocking screen.
    private suspend fun ensureCachedLibrary(url: String) {
        if (library != null) return
        val c = ServerClient(url)
        val lib = Library(c, cacheFile)
        if (lib.loadCached()) {
            client = c
            library = lib
        }
    }

    private suspend fun connect(url: String): Boolean {
        val c = client?.takeIf { it.address == url } ?: ServerClient(url)
        // One real login so the session cookie exists for the routes with no
        // LAN bypass (/playlists/cover). Failure here isn't fatal on the LAN.
        runCatching { c.login("") }
        val lib = library?.takeIf { client === c } ?: Library(c, cacheFile).also { it.loadCached() }
        client = c
        library = lib
        return try {
            lib.load()
            lastLibraryLoadAt = System.currentTimeMillis()
            lastRefreshedAt = lastLibraryLoadAt
            // A launch's own load covers the request from coming to the
            // foreground; don't reload again at the next heartbeat.
            reloadRequested = false
            true
        } catch (e: Exception) {
            false
        }
    }

    suspend fun run() {
        // Open on the last library straight away; the first probe refreshes it.
        ensureCachedLibrary(settings.serverUrl)
        while (true) {
            val url = settings.serverUrl

            if (manualSetup) {
                state = ConnState.NeedsAddress(url, "Change the address below, or keep waiting for this one.")
                waitForPoke(null)
                continue
            }

            if (!networkAvailable()) {
                ensureCachedLibrary(url)
                state = ConnState.DeviceOffline
                waitForPoke(15_000)
                continue
            }

            val wasDown = state !is ConnState.Online
            when (val result = probeServer(url)) {
                is ProbeResult.Ok -> {
                    val needsLoad = library == null || client?.address != url || library?.fromCache == true
                    val due = System.currentTimeMillis() - lastLibraryLoadAt >= LibraryRefreshMs
                    val ok = when {
                        needsLoad -> connect(url)
                        (wasDown || reloadRequested || due) && !refreshPaused -> { reloadLibrary(); true }
                        else -> {
                            // Mid-playback: progress only; the full reload
                            // waits for the next heartbeat after it ends.
                            if (wasDown) runCatching { library?.refreshProgress() }
                            true
                        }
                    }
                    if (ok) {
                        attempt = 0
                        settings.lastGoodUrl = url
                        settings.lastConnectedAt = System.currentTimeMillis()
                        state = ConnState.Online
                        // Heartbeat: notice a server that goes away mid-session.
                        waitForPoke(60_000)
                    } else {
                        goDown(url)
                    }
                }
                is ProbeResult.Unreachable -> {
                    // The built-in address is trusted even before it has ever
                    // answered: on a first launch with the server not up
                    // yet, wait for it rather than asking for an address.
                    if (url == settings.lastGoodUrl || url == settings.defaultUrl) goDown(url)
                    else {
                        state = ConnState.NeedsAddress(url, "Nothing answered at $url.")
                        waitForPoke(null)
                    }
                }
                is ProbeResult.NotStream -> {
                    state = ConnState.NeedsAddress(url, "Something answered at $url, but it isn't Stream Videos. ${result.detail}.")
                    waitForPoke(null)
                }
                is ProbeResult.Unauthorized -> {
                    state = ConnState.NeedsAddress(
                        url,
                        "The server wants a sign-in, which means it doesn't see this TV as on the home network.",
                    )
                    waitForPoke(null)
                }
            }
        }
    }

    private suspend fun goDown(url: String) {
        attempt++
        ensureCachedLibrary(url)
        val wait = backoffMs(attempt)
        state = ConnState.Reconnecting(attempt, System.currentTimeMillis() + wait)
        waitForPoke(wait)
    }
}
