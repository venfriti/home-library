package com.venfriti.stream.ui.music

import android.view.LayoutInflater
import androidx.activity.compose.BackHandler
import androidx.compose.animation.core.animateDpAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.foundation.focusGroup
import androidx.compose.foundation.focusable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.Alignment
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.shadow
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusProperties
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.LifecycleEventObserver
import androidx.lifecycle.compose.LocalLifecycleOwner
import androidx.media3.ui.AspectRatioFrameLayout
import androidx.media3.ui.PlayerView
import androidx.tv.material3.Text
import com.venfriti.stream.R
import com.venfriti.stream.ui.common.ProgressBar
import com.venfriti.stream.ui.player.TransportOverlay
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.TextColor

val MiniWidth = 256.dp
val MiniHeight = 144.dp
private val MiniMargin = 36.dp

// One video layer over the whole app. Its PlayerView never leaves
// composition: full screen and the corner card are the same view with
// animated bounds, so minimizing never re-creates the surface (on TV boxes
// that shows as a black flash).
@androidx.annotation.OptIn(androidx.media3.common.util.UnstableApi::class)
@OptIn(ExperimentalComposeUiApi::class)
@Composable
fun MusicVideoLayer(
    playback: MusicPlayback,
    // Bumped by Back on Home: focus the corner card (see below).
    focusMiniTick: Int = 0,
    onMiniFocusChanged: (Boolean) -> Unit = {},
    // Hand focus back to the page underneath (the tile you were on).
    onReturnToPage: () -> Unit = {},
) {
    if (!playback.active) return
    val entry = playback.current ?: return
    val full = playback.mode == MusicMode.Full
    val lifecycleOwner = LocalLifecycleOwner.current

    // The corner card only ever takes focus through Back (never by D-pad
    // browsing, so it stays out of the wall's way). Focused: OK = full
    // screen, Back = close, any arrow = back to the page.
    var miniArmed by remember { mutableStateOf(false) }
    var miniFocused by remember { mutableStateOf(false) }
    val miniFocus = remember { FocusRequester() }
    LaunchedEffect(focusMiniTick) {
        if (focusMiniTick > 0 && !full) {
            miniArmed = true
            withFrameNanos { }
            runCatching { miniFocus.requestFocus() }
        }
    }

    var controlsVisible by remember { mutableStateOf(true) }
    var infoOpen by remember { mutableStateOf(false) }
    var hideTick by remember { mutableStateOf(0) }
    var focusPlayTick by remember { mutableStateOf(0) }
    val rootFocus = remember { FocusRequester() }
    val playFocus = remember { FocusRequester() }
    val seekFocus = remember { FocusRequester() }

    fun poke() { controlsVisible = true; hideTick++ }

    // Home button: pause, as the film player does.
    DisposableEffect(lifecycleOwner) {
        val observer = LifecycleEventObserver { _, e -> if (e == Lifecycle.Event.ON_STOP) playback.pause() }
        lifecycleOwner.lifecycle.addObserver(observer)
        onDispose { lifecycleOwner.lifecycle.removeObserver(observer) }
    }

    // Entering full screen takes focus. Handing it back to the wall on
    // minimize/close is AppNavigation's job (it knows the screen underneath).
    // Lands on the Play/Pause button, not the bare surface behind it: the
    // transport bar is visible from this same frame (controlsVisible starts
    // true), so that's the button that's actually visible as focused (it's
    // the primary action, always gold) -- landing real focus on the surface
    // instead left Left/Right doing nothing there until a key moved it.
    LaunchedEffect(full) {
        if (full) {
            poke()
            withFrameNanos { }
            runCatching { playFocus.requestFocus() }
        }
    }
    // Controls hide after 7s idle; 15s while the info panel is open.
    LaunchedEffect(hideTick, full, infoOpen) {
        kotlinx.coroutines.delay(if (infoOpen) 15_000 else 7_000)
        if (full) { controlsVisible = false; infoOpen = false; runCatching { rootFocus.requestFocus() } }
    }
    LaunchedEffect(focusPlayTick) {
        if (focusPlayTick > 0) { withFrameNanos { }; runCatching { playFocus.requestFocus() } }
    }

    // Back in full screen minimizes -- exactly minimizePlayer() on the web.
    BackHandler(enabled = full) {
        if (infoOpen) { infoOpen = false; poke() } else playback.minimize()
    }

    BoxWithConstraints(modifier = Modifier.fillMaxSize()) {
        val w by animateDpAsState(if (full) maxWidth else MiniWidth, tween(320), label = "mvW")
        val h by animateDpAsState(if (full) maxHeight else MiniHeight, tween(320), label = "mvH")
        val x by animateDpAsState(if (full) 0.dp else maxWidth - MiniWidth - MiniMargin, tween(320), label = "mvX")
        val y by animateDpAsState(if (full) 0.dp else maxHeight - MiniHeight - MiniMargin - 22.dp, tween(320), label = "mvY")
        val corner by animateDpAsState(if (full) 0.dp else 14.dp, tween(320), label = "mvR")

        if (full) Box(modifier = Modifier.fillMaxSize().background(Color.Black))

        Box(
            modifier = Modifier
                .offset(x = x, y = y)
                .size(w, h)
                .shadow(if (full) 0.dp else 18.dp, RoundedCornerShape(corner))
                .clip(RoundedCornerShape(corner))
                .background(Color.Black)
                .then(
                    if (full) {
                        Modifier
                    } else {
                        Modifier
                            .border(2.dp, if (miniFocused) TextColor else Color.Transparent, RoundedCornerShape(corner))
                            .focusRequester(miniFocus)
                            .focusProperties { canFocus = miniArmed }
                            .onFocusChanged { f ->
                                miniFocused = f.isFocused
                                onMiniFocusChanged(f.isFocused)
                                if (!f.isFocused) miniArmed = false
                            }
                            .onPreviewKeyEvent { e ->
                                when (e.key) {
                                    // On release: expanding on press let the
                                    // release land on the full player's
                                    // Play/Pause (focused by then) and pause it.
                                    Key.DirectionCenter, Key.Enter, Key.NumPadEnter -> {
                                        if (e.type == KeyEventType.KeyUp) playback.restore()
                                        true
                                    }
                                    Key.DirectionUp, Key.DirectionDown, Key.DirectionLeft, Key.DirectionRight -> {
                                        if (e.type == KeyEventType.KeyDown) onReturnToPage()
                                        true
                                    }
                                    // Back = close, on the key itself (on TV,
                                    // Compose can act on Back before a
                                    // BackHandler sees it -- the side menu's
                                    // Back failed that way).
                                    Key.Back -> {
                                        if (e.type == KeyEventType.KeyUp) {
                                            playback.close()
                                            onReturnToPage()
                                        }
                                        true
                                    }
                                    else -> false
                                }
                            }
                            .focusable()
                    },
                ),
        ) {
            AndroidView(
                factory = { ctx ->
                    (LayoutInflater.from(ctx).inflate(R.layout.music_player_view, null) as PlayerView).apply {
                        player = playback.player
                    }
                },
                update = { view ->
                    view.resizeMode = if (full) AspectRatioFrameLayout.RESIZE_MODE_FIT else AspectRatioFrameLayout.RESIZE_MODE_ZOOM
                },
                modifier = Modifier.fillMaxSize(),
            )
            if (!full) {
                ProgressBar(
                    progress = if (playback.durationMs > 0) playback.positionMs.toDouble() / playback.durationMs else 0.0,
                    color = GoldTextColor,
                    modifier = Modifier.align(Alignment.BottomStart),
                )
            }
        }

        if (!full) {
            // Caption under the card; while the card has focus (via Back), a
            // hint of what OK and Back do there instead of the artist line.
            Column(
                modifier = Modifier
                    .offset(x = x, y = y + MiniHeight + 6.dp)
                    .size(MiniWidth, 40.dp),
            ) {
                Text(
                    text = entry.title ?: entry.name,
                    color = TextColor,
                    fontSize = 12.sp,
                    fontWeight = FontWeight.Medium,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier
                        .background(Brush.horizontalGradient(listOf(Color(0xCC0A0A0A), Color(0x990A0A0A))), RoundedCornerShape(4.dp))
                        .padding(horizontal = 6.dp, vertical = 1.dp),
                )
                Text(
                    text = if (miniFocused) {
                        "OK · FULL SCREEN    BACK · CLOSE"
                    } else {
                        listOfNotNull(entry.artist, if (!playback.isPlaying) "Paused" else null).joinToString("  ·  ").uppercase()
                    },
                    color = if (miniFocused) TextColor else MutedColor,
                    fontFamily = MonoFontFamily,
                    fontSize = 9.sp,
                    letterSpacing = 1.sp,
                    maxLines = 1,
                    modifier = Modifier.padding(start = 6.dp, top = 2.dp),
                )
            }
        }

        if (full) {
            // Focus stays inside the player while it's full screen: without
            // this, a D-pad press could walk onto the cards hidden behind it.
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .focusProperties { exit = { FocusRequester.Cancel } }
                    .focusGroup()
                    .onPreviewKeyEvent { e ->
                        if (e.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                        when (e.key) {
                            Key.MediaPlayPause -> { playback.togglePlay(); poke(); return@onPreviewKeyEvent true }
                            Key.MediaNext -> { playback.next(); poke(); return@onPreviewKeyEvent true }
                            Key.MediaPrevious -> { playback.previous(); poke(); return@onPreviewKeyEvent true }
                            Key.MediaFastForward -> { playback.seekBy(30_000); poke(); return@onPreviewKeyEvent true }
                            Key.MediaRewind -> { playback.seekBy(-30_000); poke(); return@onPreviewKeyEvent true }
                            Key.Back -> return@onPreviewKeyEvent false
                            else -> {}
                        }
                        if (!controlsVisible) {
                            when (e.key) {
                                Key.DirectionLeft -> { playback.seekBy(-10_000); poke(); true }
                                Key.DirectionRight -> { playback.seekBy(10_000); poke(); true }
                                else -> { poke(); focusPlayTick++; true }
                            }
                        } else {
                            poke()
                            false
                        }
                    },
            ) {
                Box(
                    modifier = Modifier
                        .fillMaxSize()
                        .focusRequester(rootFocus)
                        .onPreviewKeyEvent { e ->
                            if (e.type == KeyEventType.KeyDown && controlsVisible) {
                                when (e.key) {
                                    Key.DirectionUp, Key.DirectionDown -> { runCatching { seekFocus.requestFocus() }; true }
                                    Key.DirectionCenter, Key.Enter -> { playback.togglePlay(); true }
                                    else -> false
                                }
                            } else {
                                false
                            }
                        }
                        .focusable(),
                )
                TransportOverlay(
                    visible = controlsVisible,
                    entry = entry,
                    positionMs = playback.positionMs,
                    bufferedMs = playback.bufferedMs,
                    durationMs = playback.durationMs,
                    isPlaying = playback.isPlaying,
                    infoOpen = infoOpen,
                    playFocusRequester = playFocus,
                    seekFocusRequester = seekFocus,
                    hasNext = playback.hasNext,
                    onSeekTo = { playback.seekTo(it); poke() },
                    onPlayPause = { playback.togglePlay(); poke() },
                    onSeekBy = { playback.seekBy(it); poke() },
                    onNext = { playback.next(); poke() },
                    onOpenTracks = {},
                    onToggleInfo = { infoOpen = !infoOpen; poke() },
                    onBack = { if (infoOpen) { infoOpen = false; poke() } else playback.minimize() },
                    showTrackButtons = false,
                    showSeekButtons = false,
                    showSkipButtons = true,
                    hasPrevious = true,
                    onPrevious = { playback.previous(); poke() },
                    onMinimize = { playback.minimize() },
                )
                // Appears by itself in the song's last 15s (whether or not the
                // bar is up, which auto-hides after 5s), so it sits on its own
                // dark pill rather than relying on the bar's top scrim.
                val nearEnd = playback.durationMs > 0 && playback.durationMs - playback.positionMs <= 15_000
                if (playback.hasNext && nearEnd) {
                    playback.queue.getOrNull(playback.index + 1)?.let { n ->
                        Text(
                            text = "NEXT · " + listOfNotNull(n.title ?: n.name, n.artist).joinToString(" — ").uppercase(),
                            color = TextColor.copy(alpha = 0.85f),
                            fontFamily = MonoFontFamily,
                            fontSize = 10.sp,
                            letterSpacing = 1.sp,
                            maxLines = 1,
                            overflow = TextOverflow.Ellipsis,
                            modifier = Modifier
                                .align(Alignment.TopEnd)
                                .padding(top = 28.dp, end = 48.dp)
                                .widthIn(max = 520.dp)
                                .background(Color.Black.copy(alpha = 0.55f), RoundedCornerShape(50))
                                .padding(horizontal = 12.dp, vertical = 6.dp),
                        )
                    }
                }
                playback.error?.let {
                    Text(it, color = Color(0xFFE0A07A), fontSize = 14.sp, modifier = Modifier.align(Alignment.Center))
                }
            }
        }
    }
}
