package com.venfriti.stream.ui.player

import android.graphics.Bitmap
import android.graphics.drawable.BitmapDrawable
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.core.animateDpAsState
import androidx.compose.foundation.Canvas
import androidx.compose.ui.graphics.CompositingStrategy
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.input.key.onPreviewKeyEvent
import com.venfriti.stream.ui.theme.Bg3Color
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.IntOffset
import androidx.compose.ui.unit.IntSize
import coil.Coil
import coil.request.ImageRequest
import com.venfriti.stream.data.TrickplayInfo
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.animation.slideInHorizontally
import androidx.compose.animation.slideOutHorizontally
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.focusable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.wrapContentSize
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.media3.common.Tracks
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.ui.common.Artwork
import com.venfriti.stream.ui.common.ButtonKind
import com.venfriti.stream.ui.common.FocusSurface
import com.venfriti.stream.ui.common.IconCircleButton
import com.venfriti.stream.ui.common.StreamButton
import com.venfriti.stream.ui.common.StreamIcon
import com.venfriti.stream.ui.common.StreamIcons
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.InkColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.SheetColor
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TextDimColor

// One selectable line in the Audio & subtitles sheet: the track's position in
// its Tracks.Group (needed to build a TrackSelectionOverride), a label, and
// whether it's the one currently playing.
data class TrackOption(
    val group: Tracks.Group,
    val indexInGroup: Int,
    val label: String,
    val meta: String,
    val selected: Boolean,
)

fun fmtTime(ms: Long): String {
    if (ms <= 0) return "0:00"
    val totalSeconds = ms / 1000
    val h = totalSeconds / 3600
    val m = (totalSeconds % 3600) / 60
    val s = totalSeconds % 60
    return if (h > 0) "%d:%02d:%02d".format(h, m, s) else "%d:%02d".format(m, s)
}

private val PlayerSheet = Color(0xFF1A1D1D)
private val SideSheet = Color(0xFF151818)

// ---- Transport -------------------------------------------------------------
// Video is the whole screen; the chrome is a temporary overlay (MUBI/Apple
// TV) built from Material 3 parts: a real focusable slider, a filled play
// button, tonal icon buttons, all tinted with the Stream gold.
@Composable
fun TransportOverlay(
    visible: Boolean,
    entry: FileEntry,
    positionMs: Long,
    bufferedMs: Long,
    durationMs: Long,
    isPlaying: Boolean,
    infoOpen: Boolean,
    playFocusRequester: FocusRequester,
    seekFocusRequester: FocusRequester,
    hasNext: Boolean,
    onSeekTo: (Long) -> Unit,
    onPlayPause: () -> Unit,
    onSeekBy: (Long) -> Unit,
    onNext: () -> Unit,
    // Called with the button that opened the sheet, so closing it can hand
    // focus back to that button rather than Play/Pause.
    onOpenTracks: (returnFocusTo: FocusRequester) -> Unit,
    onToggleInfo: () -> Unit,
    onBack: () -> Unit,
    // Music videos: no subtitle/audio picking, no ±10s buttons, and a
    // "Keep browsing" pill that does what Back does (minimize).
    showTrackButtons: Boolean = true,
    showSeekButtons: Boolean = true,
    // Episodes and music keep Previous/Next in the row even when there's
    // nothing that way (greyed out); films, with no such thing, hide them.
    showSkipButtons: Boolean = false,
    hasPrevious: Boolean = false,
    onPrevious: () -> Unit = {},
    onMinimize: (() -> Unit)? = null,
    // Scrub preview drawn above the seek bar while scrubbing; null = none.
    scrubPreview: (@Composable (positionMs: Long) -> Unit)? = null,
    // TV episodes: an Episodes button; while it has focus the controls lift
    // and a few stills (`peek`) show the compartment underneath. OK or Down
    // on it opens the tray. The compartment never shows any other way.
    // With the tray open, focus can move between the controls and the tray:
    // Down from the controls enters it, Up from the controls closes it, and
    // the controls only dim while focus is in the tray.
    hasEpisodes: Boolean = false,
    episodesOpen: Boolean = false,
    dimmed: Boolean = false,
    peek: List<String?> = emptyList(),
    episodesFocusRequester: FocusRequester = remember { FocusRequester() },
    onOpenEpisodes: () -> Unit = {},
    // True if it moved focus into the tray; false lets the key fall through
    // to normal focus movement.
    onEnterEpisodes: () -> Boolean = { false },
    onCloseEpisodes: () -> Unit = {},
) {
    // Kept outside AnimatedVisibility: its content is dropped while the
    // sheet is open, and a requester remembered in there would be a new,
    // unattached one by the time focus is handed back.
    val subtitlesFocus = remember { FocusRequester() }
    val audioFocus = remember { FocusRequester() }
    var episodesButtonFocused by remember { mutableStateOf(false) }
    val peeking = hasEpisodes && episodesButtonFocused && !episodesOpen
    val bottomPad by animateDpAsState(
        when {
            episodesOpen -> EpisodesTrayHeight + 14.dp
            peeking -> 72.dp
            else -> 22.dp
        },
        label = "trayLift",
    )
    AnimatedVisibility(visible = visible, enter = fadeIn(), exit = fadeOut()) {
        Box(modifier = Modifier.fillMaxSize()) {
            // Top and bottom scrims only -- the middle of the frame stays clear.
            Box(
                modifier = Modifier.fillMaxSize().background(
                    Brush.verticalGradient(
                        0f to Color.Black.copy(alpha = 0.7f),
                        0.22f to Color.Transparent,
                        0.5f to Color.Transparent,
                        1f to Color.Black.copy(alpha = 0.9f),
                    ),
                ),
            )

            // Start inset so the arrow glyph (not its focus circle) lines up
            // with the 48dp gutter the seek bar and buttons use.
            Row(
                modifier = Modifier.padding(start = 37.dp, top = 28.dp),
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                IconCircleButton(StreamIcons.Back, "Back", onClick = onBack, size = 44.dp, iconSize = 22.dp, quiet = true)
                Column(verticalArrangement = Arrangement.spacedBy(3.dp)) {
                    Text(entry.title ?: entry.name, color = Color.White, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 24.sp)
                    val sub = when {
                        entry.mediaType == "tv" && entry.season > 0 ->
                            "S${"%02d".format(entry.season)}E${"%02d".format(entry.episode)}" + (entry.episodeTitle?.let { " · $it" } ?: "")
                        entry.isMusicVideo -> entry.artist
                        else -> entry.year.takeIf { it > 0 }?.toString()
                    }
                    if (!sub.isNullOrEmpty()) Text(sub, color = TextDimColor, fontSize = 14.sp)
                }
            }

            Column(
                modifier = Modifier
                    .align(Alignment.BottomStart)
                    .fillMaxWidth()
                    // ModulateAlpha fades each draw instead of rendering to an
                    // offscreen layer: that layer was cropped to this column,
                    // cutting off the left edge of a focused (zoomed) button.
                    .graphicsLayer {
                        alpha = if (dimmed) 0.6f else 1f
                        compositingStrategy = CompositingStrategy.ModulateAlpha
                    }
                    .padding(start = 48.dp, end = 48.dp, top = 22.dp, bottom = bottomPad),
            ) {
                if (infoOpen) InfoPanel(entry)

                SeekBar(
                    positionMs = positionMs,
                    bufferedMs = bufferedMs,
                    durationMs = durationMs,
                    focusRequester = seekFocusRequester,
                    onCommit = onSeekTo,
                    onPlayPause = onPlayPause,
                    preview = scrubPreview,
                )

                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(top = 12.dp)
                        .height(64.dp)
                        .onPreviewKeyEvent { e ->
                            if (!hasEpisodes || e.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                            when {
                                episodesOpen && e.key == Key.DirectionUp -> { onCloseEpisodes(); true }
                                episodesOpen && e.key == Key.DirectionDown -> onEnterEpisodes()
                                // Tray shut: only Down on the Episodes button opens it.
                                episodesButtonFocused && e.key == Key.DirectionDown -> { onOpenEpisodes(); true }
                                else -> false
                            }
                        },
                ) {
                    Row(modifier = Modifier.weight(1f)) {
                        if (hasEpisodes) {
                            StreamButton(
                                label = "Episodes",
                                trailingIcon = StreamIcons.ChevronDown,
                                mono = false,
                                height = 36.dp,
                                onClick = { if (episodesOpen) onEnterEpisodes() else onOpenEpisodes() },
                                modifier = Modifier
                                    .focusRequester(episodesFocusRequester)
                                    .onFocusChanged { episodesButtonFocused = it.isFocused },
                            )
                        }
                        if (onMinimize != null) {
                            StreamButton(label = "Keep browsing", icon = StreamIcons.Minimize, mono = false, height = 36.dp, onClick = onMinimize)
                        }
                    }
                    Row(horizontalArrangement = Arrangement.spacedBy(14.dp), verticalAlignment = Alignment.CenterVertically) {
                        if (showSkipButtons || hasPrevious) {
                            IconCircleButton(StreamIcons.SkipPrevious, "Previous", onClick = onPrevious, size = 44.dp, iconSize = 18.dp, enabled = hasPrevious)
                        }
                        if (showSeekButtons) IconCircleButton(StreamIcons.Restart, "Back 10 seconds", onClick = { onSeekBy(-10_000) }, size = 44.dp, iconSize = 22.dp, badge = "10")
                        IconCircleButton(
                            icon = if (isPlaying) StreamIcons.Pause else StreamIcons.Play,
                            description = if (isPlaying) "Pause" else "Play",
                            onClick = onPlayPause,
                            size = 60.dp,
                            iconSize = 24.dp,
                            primary = true,
                            modifier = Modifier.focusRequester(playFocusRequester),
                        )
                        if (showSeekButtons) IconCircleButton(StreamIcons.Forward, "Forward 10 seconds", onClick = { onSeekBy(10_000) }, size = 44.dp, iconSize = 22.dp, badge = "10")
                        if (showSkipButtons || hasNext) {
                            IconCircleButton(StreamIcons.SkipNext, "Next", onClick = onNext, size = 44.dp, iconSize = 18.dp, enabled = hasNext)
                        }
                    }
                    Row(modifier = Modifier.weight(1f), horizontalArrangement = Arrangement.spacedBy(8.dp, Alignment.End)) {
                        if (showTrackButtons) {
                            IconCircleButton(
                                StreamIcons.Subtitles, "Subtitles",
                                onClick = { onOpenTracks(subtitlesFocus) },
                                size = 36.dp,
                                modifier = Modifier.focusRequester(subtitlesFocus),
                            )
                            IconCircleButton(
                                StreamIcons.Audio, "Audio",
                                onClick = { onOpenTracks(audioFocus) },
                                size = 36.dp,
                                modifier = Modifier.focusRequester(audioFocus),
                            )
                        }
                        IconCircleButton(StreamIcons.Info, "Info", onClick = onToggleInfo, size = 36.dp)
                    }
                }
            }

            // The compartment: tops of this episode's and the next stills,
            // only while the Episodes button has focus.
            if (peeking && peek.isNotEmpty()) {
                Row(
                    horizontalArrangement = Arrangement.spacedBy(18.dp),
                    modifier = Modifier
                        .align(Alignment.BottomStart)
                        .padding(start = 48.dp)
                        .offset(y = 62.dp)
                        .graphicsLayer { alpha = 0.7f },
                ) {
                    peek.take(6).forEach { url ->
                        Box(
                            modifier = Modifier
                                .width(190.dp)
                                .height(107.dp)
                                .clip(RoundedCornerShape(6.dp))
                                .background(Bg3Color),
                        ) {
                            Artwork(url = url, description = null, modifier = Modifier.fillMaxSize())
                        }
                    }
                }
            }
        }
    }
}

// A Material 3 slider you drive with the D-pad: left/right scrubs (speeding
// up the longer you hold), OK commits the seek, moving focus away cancels.
// Scrubbing doesn't touch playback until you commit, like Apple TV.
@Composable
private fun SeekBar(
    positionMs: Long,
    bufferedMs: Long,
    durationMs: Long,
    focusRequester: FocusRequester,
    onCommit: (Long) -> Unit,
    onPlayPause: () -> Unit,
    preview: (@Composable (Long) -> Unit)? = null,
) {
    var focused by remember { mutableStateOf(false) }
    var scrubMs by remember { mutableStateOf<Long?>(null) }
    val shownMs = scrubMs ?: positionMs
    val fraction = if (durationMs > 0) (shownMs.toFloat() / durationMs).coerceIn(0f, 1f) else 0f
    val bufferedFraction = if (durationMs > 0) (bufferedMs.toFloat() / durationMs).coerceIn(0f, 1f) else 0f

    Column {
        BoxWithConstraints(
            modifier = Modifier
                .fillMaxWidth()
                .height(if (focused) 58.dp else 22.dp),
        ) {
            val trackWidth = maxWidth
            val scrubbing = scrubMs
            if (preview != null && scrubbing != null) {
                // Drawn above the bar's own bounds (no layout shift): the
                // frame at the scrub position, centred over the time bubble.
                val px = (trackWidth * fraction - PreviewWidth / 2).coerceIn(0.dp, trackWidth - PreviewWidth)
                // unbounded: the bar is only 58dp tall, and a plain size()
                // was clamped to that, squashing the 16:9 frame to ~3:1.
                Box(
                    modifier = Modifier
                        .offset(x = px, y = -(PreviewHeight + 8.dp))
                        .wrapContentSize(Alignment.TopStart, unbounded = true)
                        .size(PreviewWidth, PreviewHeight),
                ) {
                    preview(scrubbing)
                }
            }
            if (focused || scrubMs != null) {
                val bubbleWidth = 76.dp
                val x = (trackWidth * fraction - bubbleWidth / 2).coerceIn(0.dp, trackWidth - bubbleWidth)
                Box(
                    modifier = Modifier
                        .offset(x = x)
                        .width(bubbleWidth)
                        .height(24.dp)
                        .background(TextColor, RoundedCornerShape(50)),
                    contentAlignment = Alignment.Center,
                ) {
                    Text(fmtTime(shownMs), color = InkColor, fontFamily = MonoFontFamily, fontSize = 12.sp)
                }
            }
            Box(
                modifier = Modifier
                    .align(Alignment.BottomStart)
                    .fillMaxWidth()
                    .height(22.dp)
                    .semantics { contentDescription = "Seek" }
                    .onKeyEvent { e ->
                        if (e.type != KeyEventType.KeyDown || durationMs <= 0) return@onKeyEvent false
                        val repeat = e.nativeKeyEvent.repeatCount
                        val step = (10_000L * (1 + repeat / 4)).coerceAtMost(60_000L)
                        when (e.key) {
                            Key.DirectionLeft -> { scrubMs = ((scrubMs ?: positionMs) - step).coerceAtLeast(0); true }
                            Key.DirectionRight -> { scrubMs = ((scrubMs ?: positionMs) + step).coerceAtMost(durationMs); true }
                            Key.DirectionCenter, Key.Enter, Key.NumPadEnter -> {
                                val target = scrubMs
                                if (target != null) { onCommit(target); scrubMs = null } else onPlayPause()
                                true
                            }
                            else -> false
                        }
                    }
                    .onFocusChanged {
                        focused = it.isFocused
                        if (!it.isFocused) scrubMs = null
                    }
                    .focusRequester(focusRequester)
                    .focusable(),
                contentAlignment = Alignment.CenterStart,
            ) {
                val trackHeight = if (focused) 6.dp else 4.dp
                Box(modifier = Modifier.fillMaxWidth().height(trackHeight).background(TextColor.copy(alpha = 0.25f), RoundedCornerShape(50)))
                Box(modifier = Modifier.fillMaxWidth(bufferedFraction).height(trackHeight).background(TextColor.copy(alpha = 0.42f), RoundedCornerShape(50)))
                Box(modifier = Modifier.fillMaxWidth(fraction).height(trackHeight).background(GoldTextColor, RoundedCornerShape(50)))
                val thumb = if (focused) 18.dp else 0.dp
                if (focused) {
                    Box(
                        modifier = Modifier
                            .offset(x = (trackWidth * fraction - thumb / 2).coerceIn(0.dp, trackWidth - thumb))
                            .size(thumb)
                            .drawBehind {
                                drawCircle(GoldTextColor.copy(alpha = 0.35f), radius = size.minDimension / 2 + 6.dp.toPx())
                                drawCircle(TextColor)
                            },
                    )
                }
            }
        }
        Row(modifier = Modifier.fillMaxWidth().padding(top = 6.dp)) {
            Text(fmtTime(shownMs), color = TextDimColor, fontFamily = MonoFontFamily, fontSize = 12.sp)
            Spacer(Modifier.weight(1f))
            Text("−" + fmtTime((durationMs - shownMs).coerceAtLeast(0)), color = TextDimColor, fontFamily = MonoFontFamily, fontSize = 12.sp)
        }
    }
}

private val PreviewWidth = 192.dp
private val PreviewHeight = 108.dp

// One tile of a trickplay sprite sheet (see handleLocalTrickplay in main.go).
// Sheets are decoded as RGB_565 (~7MB each for 8x8 of 320x180) and at most
// two are held here, so scrubbing across a sheet boundary doesn't flash
// empty while the next one loads.
@Composable
fun TrickplayThumb(info: TrickplayInfo, sheetUrl: (Int) -> String, positionMs: Long) {
    val perSheet = info.cols * info.rows
    if (perSheet <= 0 || info.count <= 0) return
    val tile = ((positionMs / 1000) / info.interval).toInt().coerceIn(0, info.count - 1)
    val sheet = tile / perSheet + 1
    val col = (tile % perSheet) % info.cols
    val row = (tile % perSheet) / info.cols

    val context = LocalContext.current
    val loaded = remember { mutableStateMapOf<Int, ImageBitmap>() }
    LaunchedEffect(sheet) {
        if (loaded[sheet] != null) return@LaunchedEffect
        // Size.ORIGINAL matters: with no view target Coil otherwise scales
        // to the display size, shrinking the 2560x1440 sheet so the tile
        // offsets below pointed at the wrong pixels (an empty box).
        val request = ImageRequest.Builder(context)
            .data(sheetUrl(sheet))
            .size(coil.size.Size.ORIGINAL)
            .allowHardware(false)
            .bitmapConfig(Bitmap.Config.RGB_565)
            .build()
        val bitmap = (Coil.imageLoader(context).execute(request).drawable as? BitmapDrawable)?.bitmap ?: return@LaunchedEffect
        val keep = loaded.keys.minByOrNull { kotlin.math.abs(it - sheet) }
        loaded.keys.filter { it != keep }.forEach { loaded.remove(it) }
        loaded[sheet] = bitmap.asImageBitmap()
    }

    val shape = RoundedCornerShape(8.dp)
    Box(
        modifier = Modifier
            .fillMaxSize()
            .clip(shape)
            .background(Color.Black, shape)
            .border(1.5.dp, TextColor.copy(alpha = 0.85f), shape),
    ) {
        val image = loaded[sheet]
        if (image != null) {
            // Tile rect from the decoded size rather than trusting tile_w/h,
            // in case the bitmap ever comes back scaled.
            val tw = image.width / info.cols
            val th = image.height / info.rows
            Canvas(modifier = Modifier.fillMaxSize()) {
                drawImage(
                    image = image,
                    srcOffset = IntOffset(col * tw, row * th),
                    srcSize = IntSize(tw, th),
                    dstSize = IntSize(size.width.toInt(), size.height.toInt()),
                )
            }
        }
    }
}

@Composable
private fun InfoPanel(entry: FileEntry) {
    Column(
        modifier = Modifier.fillMaxWidth(0.55f).padding(bottom = 18.dp),
        verticalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        val overview = entry.episodeOverview ?: entry.overview
        if (!overview.isNullOrEmpty()) {
            Text(overview, color = TextColor, fontSize = 14.sp, lineHeight = 21.sp, maxLines = 4, overflow = TextOverflow.Ellipsis)
        }
        val facts = listOfNotNull(
            entry.director?.let { "Dir. $it" },
            entry.cast?.take(3)?.joinToString(", ")?.takeIf { it.isNotEmpty() },
            entry.audioLang?.let { "Audio ${it.uppercase()}" },
            entry.durationLabel.takeIf { it.isNotEmpty() },
        )
        if (facts.isNotEmpty()) {
            Text(facts.joinToString("  ·  "), color = MutedColor, fontFamily = MonoFontFamily, fontSize = 11.sp)
        }
    }
}

// ---- Resume prompt ---------------------------------------------------------
// Material 3 dialog: ask instead of silently seeking, so a rewatch doesn't
// start at the credits. Buttons are stacked full-width (easier on a D-pad
// than side-by-side text buttons).
@Composable
fun ResumeDialog(
    title: String,
    subtitle: String?,
    positionMs: Long,
    durationMs: Long,
    onResume: () -> Unit,
    onStartOver: () -> Unit,
) {
    val resumeRequester = remember { FocusRequester() }
    LaunchedEffect(Unit) { runCatching { resumeRequester.requestFocus() } }

    Box(modifier = Modifier.fillMaxSize().background(Color(0xB8040505)), contentAlignment = Alignment.Center) {
        Column(
            modifier = Modifier
                .width(380.dp)
                .background(PlayerSheet, RoundedCornerShape(28.dp))
                .padding(start = 30.dp, end = 30.dp, top = 30.dp, bottom = 26.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Box(
                modifier = Modifier.size(32.dp).background(GoldTextColor.copy(alpha = 0.16f), CircleShape),
                contentAlignment = Alignment.Center,
            ) { StreamIcon(StreamIcons.History, tint = GoldTextColor, size = 16.dp) }
            Text("Pick up where you left off?", color = Color.White, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 22.sp, lineHeight = 28.sp)
            Text(listOfNotNull(title, subtitle).joinToString(" · "), color = Color(0xFFB8B2A8), fontSize = 14.sp, maxLines = 2)
            Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(top = 2.dp, bottom = 8.dp)) {
                val f = if (durationMs > 0) (positionMs.toFloat() / durationMs).coerceIn(0f, 1f) else 0f
                Box(modifier = Modifier.weight(1f).height(4.dp).background(TextColor.copy(alpha = 0.18f), RoundedCornerShape(50))) {
                    Box(modifier = Modifier.fillMaxWidth(f).fillMaxHeight().background(GoldTextColor, RoundedCornerShape(50)))
                }
                Text(
                    "${fmtTime(positionMs)} / ${fmtTime(durationMs)}",
                    color = MutedColor,
                    fontFamily = MonoFontFamily,
                    fontSize = 11.sp,
                    modifier = Modifier.padding(start = 10.dp),
                )
            }
            StreamButton(
                label = "Resume from ${fmtTime(positionMs)}",
                icon = StreamIcons.Play,
                kind = ButtonKind.PlayerPrimary,
                mono = false,
                height = 48.dp,
                fillWidth = true,
                modifier = Modifier.focusRequester(resumeRequester),
                onClick = onResume,
            )
            StreamButton(
                label = "Start from the beginning",
                icon = StreamIcons.Restart,
                mono = false,
                height = 48.dp,
                fillWidth = true,
                onClick = onStartOver,
            )
        }
    }
}

// ---- Audio & subtitles -----------------------------------------------------
// Material 3 modal side sheet over a dimmed (still playing) picture.
@Composable
fun TrackSheet(
    visible: Boolean,
    subtitleOptions: List<TrackOption>,
    subtitlesOff: Boolean,
    audioOptions: List<TrackOption>,
    onSelectSubtitle: (TrackOption?) -> Unit,
    onSelectAudio: (TrackOption) -> Unit,
    onClose: () -> Unit,
) {
    AnimatedVisibility(visible = visible, enter = fadeIn(), exit = fadeOut()) {
        Box(modifier = Modifier.fillMaxSize().background(Color.Black.copy(alpha = 0.45f)))
    }
    AnimatedVisibility(
        visible = visible,
        enter = slideInHorizontally { it },
        exit = slideOutHorizontally { it },
        modifier = Modifier.fillMaxSize(),
    ) {
        Box(modifier = Modifier.fillMaxSize(), contentAlignment = Alignment.CenterEnd) {
            val firstRow = remember { FocusRequester() }
            LaunchedEffect(Unit) { runCatching { firstRow.requestFocus() } }
            Column(
                modifier = Modifier
                    .width(390.dp)
                    .fillMaxHeight()
                    .background(SideSheet, RoundedCornerShape(topStart = 24.dp, bottomStart = 24.dp))
                    .padding(horizontal = 32.dp, vertical = 36.dp),
                verticalArrangement = Arrangement.spacedBy(6.dp),
            ) {
                Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(bottom = 12.dp)) {
                    Text("Audio & subtitles", color = Color.White, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 22.sp, modifier = Modifier.weight(1f))
                    IconCircleButton(StreamIcons.Close, "Close", onClick = onClose, size = 32.dp, iconSize = 14.dp)
                }
                SheetLabel("Subtitles")
                RadioRow("Off", "", selected = subtitlesOff, modifier = Modifier.focusRequester(firstRow)) { onSelectSubtitle(null) }
                subtitleOptions.forEach { opt ->
                    RadioRow(opt.label, opt.meta, selected = !subtitlesOff && opt.selected) { onSelectSubtitle(opt) }
                }
                Box(modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 10.dp).height(1.dp).background(BorderColor))
                SheetLabel("Audio")
                if (audioOptions.isEmpty()) {
                    Text("Only one audio track", color = MutedColor, fontSize = 13.sp, modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp))
                }
                audioOptions.forEach { opt ->
                    RadioRow(opt.label, opt.meta, selected = opt.selected) { onSelectAudio(opt) }
                }
                Spacer(Modifier.weight(1f))
                Text(
                    "Caption size and style follow the TV's accessibility settings.",
                    color = Color(0xFF8A857C),
                    fontSize = 12.sp,
                    lineHeight = 17.sp,
                    modifier = Modifier.padding(horizontal = 16.dp),
                )
            }
        }
    }
}

@Composable
private fun SheetLabel(text: String) {
    Text(
        text.uppercase(),
        color = GoldTextColor,
        fontFamily = MonoFontFamily,
        fontSize = 11.sp,
        letterSpacing = 2.sp,
        modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
    )
}

@Composable
private fun RadioRow(label: String, meta: String, selected: Boolean, modifier: Modifier = Modifier, onClick: () -> Unit) {
    FocusSurface(
        onClick = onClick,
        modifier = modifier.fillMaxWidth().height(44.dp),
        shape = RoundedCornerShape(50),
        containerColor = Color.Transparent,
        focusedContainerColor = TextColor,
        focusedScale = 1.03f,
        outline = false,
        contentModifier = Modifier.fillMaxSize(),
    ) { focused ->
        val fg = if (focused) InkColor else TextColor
        Row(
            modifier = Modifier.fillMaxSize().padding(horizontal = 16.dp),
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            val ring = when {
                focused -> InkColor
                selected -> GoldTextColor
                else -> Color(0xFF6E6A63)
            }
            Box(
                modifier = Modifier.size(16.dp).border(1.5.dp, ring, CircleShape),
                contentAlignment = Alignment.Center,
            ) {
                if (selected) Box(modifier = Modifier.size(7.dp).background(if (focused) InkColor else GoldTextColor, CircleShape))
            }
            Text(label, color = fg, fontSize = 15.sp, modifier = Modifier.weight(1f), maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (meta.isNotEmpty()) Text(meta, color = fg.copy(alpha = 0.7f), fontFamily = MonoFontFamily, fontSize = 10.sp)
        }
    }
}

// ---- Up next ---------------------------------------------------------------
// Appears over the last seconds of an episode with a countdown ring on
// "Play now". "Watch credits" dismisses it and cancels auto-advance.
@Composable
fun UpNextCard(
    next: FileEntry,
    secondsLeft: Int,
    windowSeconds: Int,
    onPlayNow: () -> Unit,
    onWatchCredits: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val playRequester = remember { FocusRequester() }
    LaunchedEffect(Unit) { runCatching { playRequester.requestFocus() } }
    Column(
        modifier = modifier
            .width(360.dp)
            .background(Color(0xF0181B1B), RoundedCornerShape(20.dp))
            .padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(14.dp),
    ) {
        Row(horizontalArrangement = Arrangement.spacedBy(14.dp), verticalAlignment = Alignment.CenterVertically) {
            Box(modifier = Modifier.width(128.dp).height(72.dp).background(SheetColor, RoundedCornerShape(10.dp))) {
                Artwork(
                    url = next.episodeStill ?: next.thumbnailImageUrl,
                    description = null,
                    modifier = Modifier.fillMaxSize().clip(RoundedCornerShape(10.dp)),
                )
            }
            Column(verticalArrangement = Arrangement.spacedBy(3.dp)) {
                Text("UP NEXT · ${secondsLeft}S", color = GoldTextColor, fontFamily = MonoFontFamily, fontSize = 11.sp, letterSpacing = 2.sp)
                Text(next.episodeRowTitle, color = Color.White, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 18.sp, maxLines = 2, overflow = TextOverflow.Ellipsis)
                Text(
                    "S${"%02d".format(next.season)}E${"%02d".format(next.episode)}",
                    color = MutedColor,
                    fontFamily = MonoFontFamily,
                    fontSize = 11.sp,
                )
            }
        }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            StreamButton(
                label = "Play now",
                kind = ButtonKind.PlayerPrimary,
                mono = false,
                height = 44.dp,
                modifier = Modifier.weight(1f).focusRequester(playRequester),
                fillWidth = true,
                onClick = onPlayNow,
                leading = { fg ->
                    val sweep = 360f * (secondsLeft.toFloat() / windowSeconds).coerceIn(0f, 1f)
                    Box(
                        modifier = Modifier.size(20.dp).drawBehind {
                            val stroke = 2.dp.toPx()
                            val d = size.minDimension - stroke
                            drawArc(fg.copy(alpha = 0.2f), 0f, 360f, false, Offset(stroke / 2, stroke / 2), Size(d, d), style = Stroke(stroke))
                            drawArc(fg, -90f, sweep, false, Offset(stroke / 2, stroke / 2), Size(d, d), style = Stroke(stroke, cap = StrokeCap.Round))
                        },
                    )
                },
            )
            StreamButton(label = "Watch credits", mono = false, height = 44.dp, onClick = onWatchCredits)
        }
    }
}
