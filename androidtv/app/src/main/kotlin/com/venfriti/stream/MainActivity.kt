package com.venfriti.stream

import android.app.Activity
import android.media.AudioManager
import android.os.Bundle
import android.view.KeyEvent
import android.os.SystemClock
import android.util.Log
import android.widget.Toast
import androidx.compose.foundation.focusGroup
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import com.venfriti.stream.ui.browse.LocalMiniPlayerInset
import com.venfriti.stream.ui.common.LocalScreenFocusFallback
import com.venfriti.stream.ui.common.LocalTileTracker
import com.venfriti.stream.ui.common.ScreenFocusFallback
import com.venfriti.stream.ui.common.TileTracker
import androidx.compose.ui.focus.onFocusChanged
import com.venfriti.stream.ui.music.MiniWidth
import com.venfriti.stream.ui.music.MusicMode
import com.venfriti.stream.ui.music.MusicPlayback
import com.venfriti.stream.ui.music.MusicVideoLayer
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveableStateHolder
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.foundation.layout.padding
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.compose.ui.unit.dp
import androidx.tv.material3.Surface
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.Library
import com.venfriti.stream.data.QueueMemory
import com.venfriti.stream.data.ServerSettings
import com.venfriti.stream.network.ConnState
import com.venfriti.stream.network.ConnectionController
import com.venfriti.stream.ui.connect.ConnectingScreen
import com.venfriti.stream.ui.connect.DeviceOfflineScreen
import com.venfriti.stream.ui.connect.OfflineBanner
import com.venfriti.stream.ui.connect.ReconnectScreen
import com.venfriti.stream.ui.connect.ServerSetupScreen
import com.venfriti.stream.ui.settings.SettingsScreen
import com.venfriti.stream.network.ServerClient
import com.venfriti.stream.ui.browse.HomeScreen
import com.venfriti.stream.ui.browse.SectionScreen
import com.venfriti.stream.ui.nav.NavShell
import com.venfriti.stream.ui.nav.Section
import com.venfriti.stream.ui.player.PlayerScreen
import com.venfriti.stream.ui.playlist.PlaylistPicker
import com.venfriti.stream.ui.search.SearchScreen
import com.venfriti.stream.ui.show.MovieDetailScreen
import com.venfriti.stream.ui.show.ShowDetailScreen
import com.venfriti.stream.ui.theme.StreamTheme
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

// The server address, reconnecting and "server not up yet" all live in
// ConnectionController (network/Connection.kt); the default address is
// BuildConfig.DEFAULT_SERVER_URL, overridable in Settings.

private sealed interface Screen {
    data object Home : Screen
    data class Browse(val section: Section) : Screen
    data object Search : Screen
    data object Settings : Screen
    data class Show(val showId: Int) : Screen
    data class Movie(val key: String) : Screen
    // queue: the episodes after `entry` in the same season, for Next and Up next.
    data class Player(val entry: FileEntry, val queue: List<FileEntry>, val fromStart: Boolean) : Screen
}

// Each back-stack entry gets its own id so its rememberSaveable state (scroll
// positions, last-focused tile, selected season) is kept while it's covered.
private data class Entry(val id: Int, val screen: Screen)

class MainActivity : ComponentActivity() {
    // Channel +/- on the TV's remote as this box's own volume, while the app
    // is open: the speakers hang off the box's aux out, so the TV's volume
    // does nothing for them, and the TV keeps its Volume buttons to itself
    // but passes Channel +/- through over HDMI-CEC. Handled here, above
    // Compose, so it works on every screen whatever has focus. (An
    // accessibility service can't do this system-wide: keys arriving over
    // CEC never reach one.)
    override fun dispatchKeyEvent(event: KeyEvent): Boolean {
        val direction = when (event.keyCode) {
            KeyEvent.KEYCODE_CHANNEL_UP -> AudioManager.ADJUST_RAISE
            KeyEvent.KEYCODE_CHANNEL_DOWN -> AudioManager.ADJUST_LOWER
            else -> return super.dispatchKeyEvent(event)
        }
        // Press and held-button repeats step the volume; the release is
        // swallowed with them.
        if (event.action == KeyEvent.ACTION_DOWN) {
            getSystemService(AudioManager::class.java)
                ?.adjustStreamVolume(AudioManager.STREAM_MUSIC, direction, AudioManager.FLAG_SHOW_UI)
        }
        return true
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            StreamTheme {
                Surface(modifier = Modifier.fillMaxSize()) {
                    StreamApp()
                }
            }
        }
    }
}

@Composable
private fun StreamApp() {
    val context = LocalContext.current
    val controller = remember { ConnectionController(context, ServerSettings(context)) }
    val lifecycleOwner = LocalLifecycleOwner.current

    LaunchedEffect(controller) { controller.run() }
    DisposableEffect(controller) {
        val unregister = controller.watchNetwork()
        onDispose { unregister() }
    }
    // Back from standby / another app: the server may have gone (or come
    // back) meanwhile, so check now rather than at the next heartbeat.
    DisposableEffect(lifecycleOwner) {
        val observer = LifecycleEventObserver { _, event ->
            // ...and pick up anything added to the library while away.
            if (event == Lifecycle.Event.ON_START) controller.requestLibraryRefresh()
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }

    val state = controller.state
    val client = controller.client
    val library = controller.library
    when {
        state is ConnState.NeedsAddress -> ServerSetupScreen(controller, state)
        // A library on hand (live or the on-TV copy) means the wall is
        // usable, whatever the server is doing; the banner says the rest.
        // That includes the first seconds of launch: the cached copy opens the
        // wall at once while the first check runs.
        client != null && library != null && !library.loading ->
            AppNavigation(client, library, controller)
        state is ConnState.Reconnecting -> ReconnectScreen(controller, state)
        state is ConnState.DeviceOffline -> DeviceOfflineScreen(controller)
        else -> ConnectingScreen()
    }
}

@OptIn(ExperimentalComposeUiApi::class)
@Composable
private fun AppNavigation(client: ServerClient, library: Library, controller: ConnectionController) {
    var nextId by remember { mutableStateOf(1) }
    val stack = remember { mutableStateListOf(Entry(0, Screen.Home)) }
    val saveable = rememberSaveableStateHolder()
    val scope = rememberCoroutineScope()
    var addTarget by remember { mutableStateOf<FileEntry?>(null) }
    val context = LocalContext.current

    // Music videos play here, at app level, so they can shrink into the
    // corner and keep going while you browse (the web's minimizePlayer()).
    val queueMemory = remember { QueueMemory(context) }
    val music = remember(client) {
        MusicPlayback(
            context,
            client,
            onProgress = { k, pos, dur -> library.applyLocalProgress(k, pos, dur) },
            onPlayed = { e, q -> queueMemory.recordPlayed(e.key, q.map { it.key }) },
        )
    }
    DisposableEffect(music) { onDispose { music.release() } }
    val screenFallback = remember { ScreenFocusFallback() }
    val tileTracker = remember { TileTracker() }
    var pageHasFocus by remember { mutableStateOf(false) }

    // Asks a tile for focus and reports whether a tile really got it: "the
    // page has focus" isn't proof, since a request can end up stranded on a
    // group with no focused child (what a stale focusRestorer redirect did).
    suspend fun focusTile(req: FocusRequester): Boolean {
        val before = tileTracker.focusEvents
        runCatching { req.requestFocus() }
        withFrameNanos { }
        return tileTracker.focusEvents != before && pageHasFocus
    }

    // Put focus back on the page, never on nothing: the last tile you were
    // on, else the page's natural first stop.
    suspend fun focusPage() {
        val last = tileTracker.lastFocused
        if (last != null && focusTile(last)) return
        screenFallback.requester?.let { runCatching { it.requestFocus() } }
        withFrameNanos { }
        Log.d("StreamFocus", "focusPage: fell back to the page's first stop (pageHasFocus=$pageHasFocus)")
    }

    // After minimizing: the tile of the song now playing (scrolled into view
    // if the page can), else the page as above.
    suspend fun focusNowPlaying() {
        val key = music.current?.key
        if (key != null) {
            var req = tileTracker.requesterFor(key)
            if (req == null && tileTracker.reveal?.invoke(key) == true) {
                withFrameNanos { }
                withFrameNanos { }
                req = tileTracker.requesterFor(key)
            }
            if (req != null && focusTile(req)) return
            Log.d("StreamFocus", "focusNowPlaying: no tile for $key took focus (registered=${req != null})")
        }
        focusPage()
    }

    // Leaving full-screen music: minimized -> the now-playing song's tile;
    // the list running out -> back to the page.
    var lastMusicMode by remember { mutableStateOf(MusicMode.Hidden) }
    LaunchedEffect(music.mode) {
        val was = lastMusicMode
        lastMusicMode = music.mode
        if (was == MusicMode.Full && music.mode != MusicMode.Full) {
            withFrameNanos { }
            if (music.mode == MusicMode.Mini) focusNowPlaying() else focusPage()
        }
    }

    fun push(screen: Screen) {
        stack.add(Entry(nextId++, screen))
    }

    fun pop() {
        if (stack.size <= 1) return
        val removed = stack.removeAt(stack.lastIndex)
        saveable.removeState("entry-${removed.id}")
        if (removed.screen is Screen.Player) {
            // The player already patched progress locally; this reconciles
            // with the server once its final POST has had time to land.
            scope.launch { delay(1500); runCatching { library.refreshProgress() } }
        }
    }

    fun replaceTop(screen: Screen) {
        val removed = stack.removeAt(stack.lastIndex)
        saveable.removeState("entry-${removed.id}")
        push(screen)
    }

    fun queueFor(entry: FileEntry): List<FileEntry> {
        if (entry.mediaType != "tv") return emptyList()
        val show = library.show(Library.showId(entry)) ?: return emptyList()
        val season = show.episodes.filter { it.season == entry.season }.sortedBy { it.episode }
        val i = season.indexOfFirst { it.key == entry.key }
        return if (i >= 0) season.drop(i + 1) else emptyList()
    }

    // The episode before this one in the same season; null at a season's
    // first episode so Previous never jumps back across seasons.
    fun previousFor(entry: FileEntry): FileEntry? {
        if (entry.mediaType != "tv") return null
        val show = library.show(Library.showId(entry)) ?: return null
        val season = show.episodes.filter { it.season == entry.season }.sortedBy { it.episode }
        val i = season.indexOfFirst { it.key == entry.key }
        return if (i > 0) season[i - 1] else null
    }

    // fromContinue: picked from Continue watching (or the hero, which is its
    // first item). Episodes already resume in their season either way; a
    // music video resumes in the list it was actually played from.
    fun play(entry: FileEntry, fromStart: Boolean = false, list: List<FileEntry>? = null, fromContinue: Boolean = false) {
        // Browsing works from the on-TV copy; playback needs the server.
        if (controller.state !is ConnState.Online) {
            val msg = if (controller.state is ConnState.Checking) "Still connecting to the server…"
            else "The server is offline — this will play once it's back"
            Toast.makeText(context, msg, Toast.LENGTH_SHORT).show()
            controller.retryNow()
            return
        }
        if (entry.isMusicVideo) {
            // Next / auto-advance walk the list it was picked from; from
            // Continue watching, the list it was originally played from; the
            // music videos in library order when there's neither.
            val queue = when {
                list != null -> list.filter { it.isMusicVideo }.takeIf { it.isNotEmpty() }
                fromContinue -> queueMemory.queueFor(entry.key)
                    ?.mapNotNull { library.byKey(it) }
                    ?.filter { it.isMusicVideo }
                    ?.takeIf { q -> q.any { it.key == entry.key } }
                else -> null
            } ?: library.musicVideos
            // Already the one playing in the corner: carry on from where it
            // is rather than restarting. Picked from a different list, that
            // list becomes the queue.
            if (music.active && music.current?.key == entry.key) {
                if (list != null) music.adoptQueue(queue)
                music.restore()
                return
            }
            val i = queue.indexOfFirst { it.key == entry.key }
            if (i >= 0) music.start(queue, i) else music.start(listOf(entry), 0)
            return
        }
        // A film or episode takes over the screen: the music stops.
        if (music.active) music.close()
        push(Screen.Player(entry, queueFor(entry), fromStart))
    }

    fun select(section: Section) {
        while (stack.size > 1) pop()
        val root = stack[0]
        if (section == Section.Home) {
            if (root.screen != Screen.Home) replaceTop(Screen.Home)
        } else {
            push(
                when (section) {
                    Section.Search -> Screen.Search
                    Section.Settings -> Screen.Settings
                    else -> Screen.Browse(section)
                },
            )
        }
    }

    val top = stack.last()

    // Back pops the stack; from a top-level section it goes Home first, and
    // only Back on Home leaves the app. The player and dialogs handle their
    // own Back before this one sees it.
    BackHandler(enabled = stack.size > 1 && top.screen !is Screen.Player && addTarget == null && music.mode != MusicMode.Full) { pop() }

    // No background library reloads while something is playing full screen.
    SideEffect { controller.refreshPaused = top.screen is Screen.Player || music.mode == MusicMode.Full }
    // Back, in order (the side menu, the players and dialogs handle their own
    // Back first):
    // 1. on any page, a video in the corner that isn't focused -> focus it
    //    (MusicVideoLayer then takes OK = full screen, Back = close), before
    //    the page is left;
    // 2. otherwise the previous page (the handler above);
    // 3. on Home with nothing open -> "Press Back again to exit", and a
    //    second Back within 2s leaves the app.
    var focusMiniTick by remember { mutableStateOf(0) }
    var miniFocused by remember { mutableStateOf(false) }
    val miniHasFocus = miniFocused && music.mode == MusicMode.Mini
    BackHandler(enabled = music.mode == MusicMode.Mini && addTarget == null && !miniHasFocus && top.screen !is Screen.Player) {
        focusMiniTick++
    }
    var lastBackAt by remember { mutableStateOf(0L) }
    BackHandler(enabled = stack.size == 1 && music.mode == MusicMode.Hidden && addTarget == null) {
        val now = SystemClock.uptimeMillis()
        if (now - lastBackAt < 2_000) {
            (context as? Activity)?.finish()
        } else {
            lastBackAt = now
            Toast.makeText(context, "Press Back again to exit", Toast.LENGTH_SHORT).show()
        }
    }

    val sections = buildList {
        add(Section.Home)
        add(Section.Films)
        add(Section.Series)
        add(Section.Playlists)
        add(Section.Music)
        add(Section.Settings)
    }
    val counts = mapOf(
        Section.Continue to library.continueWatching.size,
        Section.Films to library.films.size,
        Section.Series to library.shows.size,
        Section.Playlists to library.playlists.size,
        Section.Music to library.musicVideos.size,
        Section.Library to library.other.size,
    )
    val currentSection = stack.asReversed().firstNotNullOfOrNull { e ->
        when (val s = e.screen) {
            is Screen.Home -> Section.Home
            is Screen.Browse -> s.section
            is Screen.Search -> Section.Search
            is Screen.Settings -> Section.Settings
            is Screen.Show -> Section.Series
            is Screen.Movie -> Section.Films
            is Screen.Player -> null
        }
    } ?: Section.Home

    val miniInset = if (music.mode == MusicMode.Mini) MiniWidth + 24.dp else 0.dp

    Box(
        modifier = Modifier
            .fillMaxSize()
            .onPreviewKeyEvent { e ->
                // Remote media keys drive the corner player from anywhere.
                if (music.mode != MusicMode.Mini || e.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                when (e.key) {
                    Key.MediaPlayPause, Key.MediaPlay, Key.MediaPause -> { music.togglePlay(); true }
                    Key.MediaNext -> { music.next(); true }
                    Key.MediaPrevious -> { music.previous(); true }
                    Key.MediaStop -> { music.close(); true }
                    else -> false
                }
            },
    ) {
      CompositionLocalProvider(
          LocalMiniPlayerInset provides miniInset,
          LocalScreenFocusFallback provides screenFallback,
          LocalTileTracker provides tileTracker,
      ) {
       Box(
           modifier = Modifier
               .fillMaxSize()
               .onFocusChanged { pageHasFocus = it.hasFocus }
               // No focusRestorer here (or on the page area in NavShell):
               // Compose consults an Inactive ancestor's restorer on *any*
               // focus request into it, and redirected "focus the playing
               // song's tile" to a stale child, leaving focus on nothing.
               // TileTracker does the remembering instead.
               .focusGroup(),
       ) {
        saveable.SaveableStateProvider("entry-${top.id}") {
            when (val screen = top.screen) {
                is Screen.Player -> key(screen.entry.key) {
                    val previous = remember(screen.entry.key) { previousFor(screen.entry) }
                    PlayerScreen(
                        client = client,
                        entry = screen.entry,
                        nextEntry = screen.queue.firstOrNull(),
                        previousEntry = previous,
                        fromStart = screen.fromStart,
                        onNext = {
                            val next = screen.queue.firstOrNull()
                            if (next != null) replaceTop(Screen.Player(next, screen.queue.drop(1), false))
                        },
                        onPrevious = {
                            if (previous != null) replaceTop(Screen.Player(previous, queueFor(previous), false))
                        },
                        episodes = if (screen.entry.mediaType == "tv") {
                            library.show(Library.showId(screen.entry))?.episodes ?: emptyList()
                        } else {
                            emptyList()
                        },
                        // Switch episode without leaving the player.
                        onPlayEpisode = { ep -> replaceTop(Screen.Player(ep, queueFor(ep), false)) },
                        onProgress = { k, pos, dur -> library.applyLocalProgress(k, pos, dur) },
                        onClose = { pop() },
                    )
                }

                else -> NavShell(
                    current = currentSection,
                    sections = sections,
                    counts = counts,
                    onSelect = ::select,
                    nowPlaying = if (music.mode == MusicMode.Mini) music.current else null,
                    onNowPlaying = { music.restore() },
                ) {
                    when (screen) {
                        is Screen.Home -> HomeScreen(
                            library = library,
                            onPlay = { play(it) },
                            onResume = { play(it, fromContinue = true) },
                            onOpenShow = { push(Screen.Show(it.tmdbId)) },
                            onOpenMovie = { push(Screen.Movie(it.key)) },
                            onOpenSection = { select(it) },
                            onLongPress = { addTarget = it },
                        )
                        is Screen.Browse -> SectionScreen(
                            section = screen.section,
                            library = library,
                            onPlay = { play(it) },
                            onOpenShow = { push(Screen.Show(it.tmdbId)) },
                            onOpenMovie = { push(Screen.Movie(it.key)) },
                            onLongPress = { addTarget = it },
                            onPlayQueue = { f, list -> play(f, list = list) },
                        )
                        is Screen.Search -> SearchScreen(
                            library = library,
                            onPlay = { play(it) },
                            onOpenShow = { push(Screen.Show(it.tmdbId)) },
                        )
                        is Screen.Show -> {
                            val show = library.show(screen.showId)
                            if (show == null) {
                                LaunchedEffect(Unit) { pop() }
                            } else {
                                ShowDetailScreen(
                                    show = show,
                                    onPlay = { ep, fromStart -> play(ep, fromStart) },
                                    onAddToPlaylist = { addTarget = it },
                                )
                            }
                        }
                        is Screen.Movie -> {
                            val film = library.byKey(screen.key)
                            if (film == null) {
                                LaunchedEffect(Unit) { pop() }
                            } else {
                                MovieDetailScreen(
                                    entry = film,
                                    onPlay = { e, fromStart -> play(e, fromStart) },
                                    onAddToPlaylist = { addTarget = it },
                                )
                            }
                        }
                        is Screen.Settings -> SettingsScreen(controller)
                        is Screen.Player -> Unit
                    }
                }
            }
        }

       }
      }

        if (top.screen !is Screen.Player) {
            OfflineBanner(controller.state, modifier = Modifier.align(Alignment.TopEnd).padding(top = 24.dp, end = 48.dp))
        }

        MusicVideoLayer(
            music,
            focusMiniTick = focusMiniTick,
            onMiniFocusChanged = { miniFocused = it },
            onReturnToPage = {
                miniFocused = false
                scope.launch { focusPage() }
            },
        )

        addTarget?.let { target ->
            PlaylistPicker(
                entry = target,
                playlists = library.playlists,
                onToggle = { playlist, contains ->
                    scope.launch {
                        runCatching {
                            if (contains) client.removeFromPlaylist(playlist.id, target.key)
                            else client.addToPlaylist(playlist.id, target.key)
                        }
                        library.refreshPlaylists()
                    }
                },
                onCreate = { name ->
                    scope.launch {
                        runCatching { client.createPlaylist(name, target.key) }
                        library.refreshPlaylists()
                    }
                },
                onDismiss = { addTarget = null },
            )
        }
    }
}
