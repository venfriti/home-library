package com.venfriti.stream.ui.player

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.focusable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.media3.common.AudioAttributes
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.MimeTypes
import androidx.media3.common.Player
import androidx.media3.common.TrackSelectionOverride
import androidx.media3.common.Tracks
import androidx.media3.exoplayer.DefaultRenderersFactory
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.ui.PlayerView
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.PlaybackProgress
import com.venfriti.stream.data.TrackPrefs
import com.venfriti.stream.data.TrickplayInfo
import com.venfriti.stream.network.ServerClient
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

// Outlives any one PlayerScreen. The old final save was launched on the
// composable's own scope from inside onDispose -- a scope that is cancelled
// at exactly that moment, so the last position on close usually never
// reached the server.
private val progressScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

private const val UpNextWindowSeconds = 20
private const val HideAfterMs = 7_000L
// Longer while the info panel is up, so its description can be read.
private const val HideAfterWithInfoMs = 12_000L

// MainActivity keys this composable by entry.key, so each episode gets a
// fresh screen and player. The listener/lifecycle/dispose closures below all
// capture `entry`; when the old screen was reused across episodes they kept
// saving progress under the *first* episode's key and the auto-advance guard
// stayed tripped after one hand-off.
//
// Resolve a playable URL, ask before resuming (music videos always start at
// 0, matching resumePlayback's fromStart rule in main.go), report progress
// every 10s, flush on close *and* when the app is backgrounded (Home button),
// and hand off to the next episode through the Up next card.
// DefaultRenderersFactory / SubtitleView styling are Media3 @UnstableApi.
@androidx.annotation.OptIn(androidx.media3.common.util.UnstableApi::class)
@Composable
fun PlayerScreen(
    client: ServerClient,
    entry: FileEntry,
    nextEntry: FileEntry? = null,
    previousEntry: FileEntry? = null,
    fromStart: Boolean = false,
    onNext: () -> Unit = {},
    onPrevious: () -> Unit = {},
    // All episodes of this show, for the tray under the controls; picking
    // one switches in place (MainActivity swaps the player entry).
    episodes: List<FileEntry> = emptyList(),
    onPlayEpisode: (FileEntry) -> Unit = {},
    onProgress: (key: String, positionSeconds: Double, durationSeconds: Double) -> Unit = { _, _, _ -> },
    onClose: () -> Unit,
) {
    val context = LocalContext.current
    val lifecycleOwner = LocalLifecycleOwner.current
    var errorMessage by remember { mutableStateOf<String?>(null) }
    var connectionLost by remember { mutableStateOf(false) }

    val exoPlayer = remember {
        ExoPlayer.Builder(context)
            // Falls back to a software decoder when the box's hardware one
            // rejects a stream, instead of failing playback outright.
            .setRenderersFactory(DefaultRenderersFactory(context).setEnableDecoderFallback(true))
            // Movie audio attributes + handleAudioFocus: pauses for Assistant
            // and other apps, and enables surround passthrough where the
            // sink supports it.
            .setAudioAttributes(
                AudioAttributes.Builder().setUsage(C.USAGE_MEDIA).setContentType(C.AUDIO_CONTENT_TYPE_MOVIE).build(),
                true,
            )
            .build()
    }

    var isPlaying by remember { mutableStateOf(false) }
    var positionMs by remember { mutableStateOf(0L) }
    var bufferedMs by remember { mutableStateOf(0L) }
    var durationMs by remember { mutableStateOf(0L) }
    var currentTracks by remember { mutableStateOf<Tracks?>(null) }
    val trackPrefs = remember { TrackPrefs(context) }
    var subtitlesOff by remember { mutableStateOf(trackPrefs.subtitles == TrackPrefs.Subtitles.Off) }

    var controlsVisible by remember { mutableStateOf(true) }
    var sheetOpen by remember { mutableStateOf(false) }
    var infoOpen by remember { mutableStateOf(false) }
    var resumePrompt by remember { mutableStateOf<PlaybackProgress?>(null) }
    var upNextDismissed by remember { mutableStateOf(false) }
    var hideTick by remember { mutableStateOf(0) }
    var focusPlayTick by remember { mutableStateOf(0) }
    var trayOpen by remember { mutableStateOf(false) }
    var trayHasFocus by remember { mutableStateOf(false) }
    var focusEpisodesTick by remember { mutableStateOf(0) }
    val hasEpisodes = entry.mediaType == "tv" && episodes.size > 1
    val episodesFocus = remember { FocusRequester() }
    val trayCurrentFocus = remember { FocusRequester() }

    val videoFocus = remember { FocusRequester() }
    val playFocus = remember { FocusRequester() }
    val seekFocus = remember { FocusRequester() }

    fun poke() {
        controlsVisible = true
        hideTick++
    }

    fun flushProgress() {
        val duration = exoPlayer.duration
        if (duration <= 0 || resumePrompt != null) return
        val pos = exoPlayer.currentPosition / 1000.0
        val dur = duration / 1000.0
        onProgress(entry.key, pos, dur)
        progressScope.launch { client.postProgress(entry.key, pos, dur) }
    }

    val currentNextEntry by rememberUpdatedState(nextEntry)
    val currentOnNext by rememberUpdatedState(onNext)
    val currentOnClose by rememberUpdatedState(onClose)
    val currentDismissed by rememberUpdatedState(upNextDismissed)
    var advanced by remember { mutableStateOf(false) }

    fun advanceToNext() {
        // STATE_ENDED can fire more than once before the queue swap lands;
        // the guard keeps onNext() from running twice for one episode.
        if (advanced || currentNextEntry == null) return
        advanced = true
        flushProgress()
        currentOnNext()
    }

    val listener = remember {
        object : Player.Listener {
            override fun onIsPlayingChanged(playing: Boolean) { isPlaying = playing }
            override fun onTracksChanged(tracks: Tracks) { currentTracks = tracks }
            override fun onPlaybackStateChanged(state: Int) {
                if (state != Player.STATE_ENDED) return
                // "Watch credits" means stay: at the very end, go back rather
                // than rolling into the next episode.
                if (currentNextEntry != null && !currentDismissed) advanceToNext() else {
                    flushProgress()
                    currentOnClose()
                }
            }
            override fun onPlayerError(error: androidx.media3.common.PlaybackException) {
                // 2xxx = network/IO: the server dropped mid-stream. Keep the
                // position and keep retrying instead of dead-ending.
                if (error.errorCode in 2000..2999) {
                    connectionLost = true
                } else {
                    errorMessage = "Playback error: ${error.errorCodeName}"
                }
            }
        }
    }
    DisposableEffect(exoPlayer) {
        exoPlayer.addListener(listener)
        onDispose { exoPlayer.removeListener(listener) }
    }

    // Home button / switching apps stops the activity without disposing this
    // composable: pause and save there too, not only on Back.
    DisposableEffect(lifecycleOwner) {
        val observer = LifecycleEventObserver { _, event ->
            if (event == Lifecycle.Event.ON_STOP) {
                exoPlayer.pause()
                flushProgress()
            }
        }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }

    // The transport bar is visible from the first frame (controlsVisible
    // starts true), so focus belongs on its Play/Pause button, not the bare
    // video surface behind it -- landing it on the surface let a Left/Right
    // press seek immediately (the surface's own onPreviewKeyEvent below)
    // while the button sat there looking focused (it's just always gold,
    // being the primary action) but wasn't. A resume prompt, if one comes
    // back from the progress check below, steals focus right after via its
    // own requester.
    LaunchedEffect(Unit) {
        withFrameNanos { }
        runCatching { playFocus.requestFocus() }
    }

    LaunchedEffect(entry.key) {
        try {
            val url = client.resolvePlaybackUrl(entry)
            val itemBuilder = MediaItem.Builder().setUri(url)
            if (!entry.subtitleUrl.isNullOrEmpty()) {
                val subtitle = MediaItem.SubtitleConfiguration.Builder(android.net.Uri.parse(entry.subtitleUrl))
                    .setMimeType(MimeTypes.TEXT_VTT)
                    .setLanguage("en")
                    .build()
                itemBuilder.setSubtitleConfigurations(listOf(subtitle))
            }
            exoPlayer.setMediaItem(itemBuilder.build())
            // Subtitles: the last hand-picked language (English as the
            // fallback when this file doesn't have it), or off if that was
            // the last choice; English by default. Audio: the last
            // hand-picked language, else the file's default track.
            val subPref = trackPrefs.subtitles
            val textLanguages = listOfNotNull((subPref as? TrackPrefs.Subtitles.Language)?.code, "en").distinct()
            exoPlayer.trackSelectionParameters = exoPlayer.trackSelectionParameters
                .buildUpon()
                .setTrackTypeDisabled(C.TRACK_TYPE_TEXT, subtitlesOff)
                .setPreferredTextLanguages(*textLanguages.toTypedArray())
                .apply { trackPrefs.audioLanguage?.let { setPreferredAudioLanguages(it) } }
                .build()

            val resume = if (fromStart || entry.isMusicVideo) null else client.getProgress(entry.key)
            // Only worth asking about when there's real progress on both sides:
            // not the first 30 seconds, not the last minute.
            val ask = resume != null && resume.position > 30 && resume.duration - resume.position > 60
            exoPlayer.playWhenReady = !ask
            exoPlayer.prepare()
            if (ask) {
                exoPlayer.seekTo((resume!!.position * 1000).toLong())
                resumePrompt = resume
                controlsVisible = false
            }
        } catch (e: Exception) {
            errorMessage = "Couldn't start playback: ${e.message}"
        }
    }

    // Scrub previews: the first ask starts generation on the server, so keep
    // asking (every 15s, for up to ~5 min) until the sheets are ready.
    var trickplay by remember { mutableStateOf<TrickplayInfo?>(null) }
    LaunchedEffect(entry.key) {
        if (!entry.isLocal) return@LaunchedEffect
        repeat(20) {
            val info = client.getTrickplay(entry.key)
            if (info?.ready == true) { trickplay = info; return@LaunchedEffect }
            delay(15_000)
        }
    }

    // Lost the server mid-play: re-prepare every 5s from the same position
    // until it answers again (a paused player just resumes where it was).
    LaunchedEffect(connectionLost) {
        while (connectionLost) {
            delay(5_000)
            exoPlayer.prepare()
            delay(3_000)
            if (exoPlayer.playerError == null && exoPlayer.playbackState != Player.STATE_IDLE) connectionLost = false
        }
    }

    LaunchedEffect(Unit) {
        while (true) {
            positionMs = exoPlayer.currentPosition
            bufferedMs = exoPlayer.bufferedPosition
            exoPlayer.duration.takeIf { it > 0 }?.let { durationMs = it }
            delay(500)
        }
    }

    // Focus lands on play only after the recomposition that shows the bar.
    LaunchedEffect(focusPlayTick) {
        if (focusPlayTick > 0) {
            withFrameNanos { }
            runCatching { playFocus.requestFocus() }
        }
    }
    // Closing the Audio & subtitles sheet: back to whichever button opened it.
    var sheetReturnFocus by remember { mutableStateOf<FocusRequester?>(null) }
    var focusSheetOpenerTick by remember { mutableStateOf(0) }
    LaunchedEffect(focusSheetOpenerTick) {
        if (focusSheetOpenerTick > 0) {
            withFrameNanos { }
            val target = sheetReturnFocus
            if (target == null || runCatching { target.requestFocus() }.isFailure) runCatching { playFocus.requestFocus() }
        }
    }
    // Same, for coming back up out of the episodes tray.
    LaunchedEffect(focusEpisodesTick) {
        if (focusEpisodesTick > 0) {
            withFrameNanos { }
            runCatching { episodesFocus.requestFocus() }
        }
    }

    LaunchedEffect(entry.key) {
        while (true) {
            delay(10_000)
            val duration = exoPlayer.duration
            if (duration > 0 && resumePrompt == null) {
                val pos = exoPlayer.currentPosition / 1000.0
                client.postProgress(entry.key, pos, duration / 1000.0)
            }
        }
    }

    DisposableEffect(Unit) {
        onDispose {
            flushProgress()
            exoPlayer.release()
        }
    }

    fun seekBy(deltaMs: Long) {
        exoPlayer.seekTo((exoPlayer.currentPosition + deltaMs).coerceAtLeast(0))
    }

    fun togglePlay() {
        exoPlayer.playWhenReady = !exoPlayer.playWhenReady
    }

    fun applyOverride(group: Tracks.Group, indexInGroup: Int) {
        exoPlayer.trackSelectionParameters = exoPlayer.trackSelectionParameters
            .buildUpon()
            .setOverrideForType(TrackSelectionOverride(group.mediaTrackGroup, indexInGroup))
            .build()
    }

    val subtitleOptions = remember(currentTracks) { buildTrackOptions(currentTracks, C.TRACK_TYPE_TEXT) }
    val audioOptions = remember(currentTracks) { buildTrackOptions(currentTracks, C.TRACK_TYPE_AUDIO) }

    val secondsLeft = if (durationMs > 0) ((durationMs - positionMs) / 1000).toInt() else Int.MAX_VALUE
    val upNextVisible = nextEntry != null && !upNextDismissed && resumePrompt == null &&
        durationMs > 0 && secondsLeft in 0..UpNextWindowSeconds
    val modal = resumePrompt != null || sheetOpen || upNextVisible || trayOpen
    val upNextNow by rememberUpdatedState(upNextVisible)

    LaunchedEffect(hideTick, sheetOpen, resumePrompt, trayOpen, infoOpen) {
        delay(if (infoOpen) HideAfterWithInfoMs else HideAfterMs)
        if (!sheetOpen && !trayOpen && resumePrompt == null && controlsVisible) {
            controlsVisible = false
            infoOpen = false
            // Don't pull focus off the Up next card if it's showing.
            if (!upNextNow) runCatching { videoFocus.requestFocus() }
        }
    }

    // Back undoes the most recent thing: sheet, then info/controls, then exits.
    BackHandler {
        when {
            sheetOpen -> { sheetOpen = false; poke(); focusSheetOpenerTick++ }
            trayOpen -> { trayOpen = false; poke(); focusEpisodesTick++ }
            resumePrompt != null -> { flushProgress(); onClose() }
            controlsVisible && !upNextVisible -> {
                controlsVisible = false
                infoOpen = false
                runCatching { videoFocus.requestFocus() }
            }
            else -> { flushProgress(); onClose() }
        }
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Color.Black)
            .onPreviewKeyEvent { event ->
                if (event.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                // Remote media keys work in every state.
                when (event.key) {
                    Key.MediaPlayPause -> { togglePlay(); poke(); return@onPreviewKeyEvent true }
                    Key.MediaPlay -> { exoPlayer.play(); poke(); return@onPreviewKeyEvent true }
                    Key.MediaPause -> { exoPlayer.pause(); poke(); return@onPreviewKeyEvent true }
                    Key.MediaFastForward -> { seekBy(30_000); poke(); return@onPreviewKeyEvent true }
                    Key.MediaRewind -> { seekBy(-30_000); poke(); return@onPreviewKeyEvent true }
                    Key.MediaNext -> { advanceToNext(); return@onPreviewKeyEvent true }
                    Key.MediaPrevious -> {
                        if (previousEntry != null) { flushProgress(); onPrevious() }
                        return@onPreviewKeyEvent true
                    }
                    else -> {}
                }
                if (modal) return@onPreviewKeyEvent false
                if (event.key == Key.Back) return@onPreviewKeyEvent false
                if (!controlsVisible) {
                    // Chrome hidden: left/right skip 10s (and show the bar
                    // without taking focus, so repeated presses keep
                    // skipping); anything else just brings the chrome back
                    // with focus on play -- it doesn't also act on the key.
                    when (event.key) {
                        Key.DirectionLeft -> { seekBy(-10_000); poke(); true }
                        Key.DirectionRight -> { seekBy(10_000); poke(); true }
                        else -> { poke(); focusPlayTick++; true }
                    }
                } else {
                    poke()
                    // Chrome shown but focus still on the bare video surface
                    // (after a left/right skip): up/down/OK hand it to the bar.
                    false
                }
            },
    ) {
        AndroidView(
            factory = {
                PlayerView(context).apply {
                    player = exoPlayer
                    useController = false
                    keepScreenOn = true
                    // Subtitles in the viewer's system caption style.
                    subtitleView?.setUserDefaultStyle()
                    subtitleView?.setUserDefaultTextSize()
                }
            },
            modifier = Modifier
                .fillMaxSize()
                .focusRequester(videoFocus)
                .onPreviewKeyEvent { event ->
                    // Focus is on the surface while the bar is visible after a
                    // skip: route up/down/OK into the controls.
                    if (event.type == KeyEventType.KeyDown && controlsVisible && !modal) {
                        when (event.key) {
                            Key.DirectionUp, Key.DirectionDown -> { runCatching { seekFocus.requestFocus() }; true }
                            Key.DirectionCenter, Key.Enter -> { togglePlay(); true }
                            Key.DirectionLeft -> { seekBy(-10_000); true }
                            Key.DirectionRight -> { seekBy(10_000); true }
                            else -> false
                        }
                    } else {
                        false
                    }
                }
                .focusable(),
        )

        TransportOverlay(
            visible = controlsVisible && !sheetOpen && resumePrompt == null,
            entry = entry,
            positionMs = positionMs,
            bufferedMs = bufferedMs,
            durationMs = durationMs,
            isPlaying = isPlaying,
            infoOpen = infoOpen,
            playFocusRequester = playFocus,
            seekFocusRequester = seekFocus,
            hasNext = nextEntry != null,
            showSkipButtons = entry.mediaType == "tv",
            hasPrevious = previousEntry != null,
            onPrevious = { flushProgress(); onPrevious() },
            scrubPreview = trickplay?.let { info ->
                { pos -> TrickplayThumb(info, { n -> client.trickplaySheetUrl(entry.key, n) }, pos) }
            },
            onSeekTo = { exoPlayer.seekTo(it); poke() },
            onPlayPause = { togglePlay(); poke() },
            onSeekBy = { seekBy(it); poke() },
            onNext = { advanceToNext() },
            hasEpisodes = hasEpisodes,
            episodesOpen = trayOpen,
            dimmed = trayOpen && trayHasFocus,
            episodesFocusRequester = episodesFocus,
            onEnterEpisodes = { runCatching { trayCurrentFocus.requestFocus() }.isSuccess },
            onCloseEpisodes = { trayOpen = false; poke() },
            peek = remember(episodes, entry.key) {
                val season = episodes.filter { it.season == entry.season }.sortedBy { it.episode }
                val i = season.indexOfFirst { it.key == entry.key }.coerceAtLeast(0)
                season.drop(i).map { it.episodeStill ?: it.thumbnailImageUrl }
            },
            onOpenEpisodes = { trayOpen = true; infoOpen = false; poke() },
            onOpenTracks = { opener -> sheetReturnFocus = opener; sheetOpen = true },
            onToggleInfo = { infoOpen = !infoOpen; poke() },
            onBack = { flushProgress(); onClose() },
        )

        if (hasEpisodes) {
            EpisodesTray(
                visible = trayOpen && controlsVisible,
                episodes = episodes,
                current = entry,
                currentRequester = trayCurrentFocus,
                onPick = { ep ->
                    trayOpen = false
                    flushProgress()
                    onPlayEpisode(ep)
                },
                onClose = { trayOpen = false; poke(); focusEpisodesTick++ },
                onUpOut = { poke(); focusEpisodesTick++ },
                onFocusInside = { trayHasFocus = it },
                modifier = Modifier.align(Alignment.BottomCenter),
            )
        }

        if (upNextVisible && nextEntry != null) {
            UpNextCard(
                next = nextEntry,
                secondsLeft = secondsLeft.coerceAtLeast(0),
                windowSeconds = UpNextWindowSeconds,
                onPlayNow = { advanceToNext() },
                onWatchCredits = {
                    upNextDismissed = true
                    poke()
                    focusPlayTick++
                },
                modifier = Modifier.align(Alignment.BottomEnd).padding(end = 48.dp, bottom = 48.dp),
            )
        }

        TrackSheet(
            visible = sheetOpen,
            subtitleOptions = subtitleOptions,
            subtitlesOff = subtitlesOff,
            audioOptions = audioOptions,
            onSelectSubtitle = { option ->
                subtitlesOff = option == null
                exoPlayer.trackSelectionParameters = exoPlayer.trackSelectionParameters
                    .buildUpon()
                    .setTrackTypeDisabled(C.TRACK_TYPE_TEXT, option == null)
                    .build()
                if (option != null) applyOverride(option.group, option.indexInGroup)
                // Remembered for every video after this one. An untagged
                // track can't be matched by language, so it falls back to
                // "on, English first".
                trackPrefs.subtitles = when {
                    option == null -> TrackPrefs.Subtitles.Off
                    else -> option.group.getTrackFormat(option.indexInGroup).language
                        ?.let { TrackPrefs.Subtitles.Language(it) } ?: TrackPrefs.Subtitles.Default
                }
            },
            onSelectAudio = { option ->
                applyOverride(option.group, option.indexInGroup)
                option.group.getTrackFormat(option.indexInGroup).language?.let { trackPrefs.audioLanguage = it }
            },
            onClose = { sheetOpen = false; poke(); focusSheetOpenerTick++ },
        )

        resumePrompt?.let { p ->
            ResumeDialog(
                title = entry.title ?: entry.name,
                subtitle = if (entry.mediaType == "tv" && entry.season > 0) {
                    "S${"%02d".format(entry.season)}E${"%02d".format(entry.episode)}" + (entry.episodeTitle?.let { " · $it" } ?: "")
                } else {
                    null
                },
                positionMs = (p.position * 1000).toLong(),
                durationMs = (p.duration * 1000).toLong(),
                onResume = {
                    resumePrompt = null
                    exoPlayer.play()
                    runCatching { videoFocus.requestFocus() }
                },
                onStartOver = {
                    resumePrompt = null
                    exoPlayer.seekTo(0)
                    exoPlayer.play()
                    runCatching { videoFocus.requestFocus() }
                },
            )
        }

        errorMessage?.let {
            Text(text = it, color = Color(0xFFE06C6C), modifier = Modifier.align(Alignment.Center))
        }
        if (connectionLost) {
            com.venfriti.stream.ui.connect.OfflineBanner(
                state = com.venfriti.stream.network.ConnState.Reconnecting(0, 0),
                modifier = Modifier.align(Alignment.TopEnd).padding(top = 36.dp, end = 48.dp),
            )
        }
    }
}

// Picker list for one track type from ExoPlayer's Tracks snapshot: language
// first, falling back to the track label or a numbered entry so untagged
// multi-audio files still get distinct rows.
private fun buildTrackOptions(tracks: Tracks?, type: Int): List<TrackOption> {
    if (tracks == null) return emptyList()
    val options = mutableListOf<TrackOption>()
    for (group in tracks.groups) {
        if (group.type != type) continue
        for (i in 0 until group.length) {
            if (!group.isTrackSupported(i)) continue
            val format = group.getTrackFormat(i)
            val language = format.language?.let { code ->
                java.util.Locale.forLanguageTag(code).displayLanguage.takeIf { it.isNotBlank() && it != code } ?: code.uppercase()
            }
            val label = language ?: format.label ?: "Track ${options.size + 1}"
            val meta = if (type == C.TRACK_TYPE_AUDIO && format.channelCount > 0) {
                when (format.channelCount) {
                    1 -> "MONO"
                    2 -> "STEREO"
                    6 -> "5.1"
                    8 -> "7.1"
                    else -> "${format.channelCount}CH"
                }
            } else {
                format.sampleMimeType?.substringAfterLast('/')?.uppercase() ?: ""
            }
            options.add(TrackOption(group = group, indexInGroup = i, label = label, meta = meta, selected = group.isTrackSelected(i)))
        }
    }
    return options
}
