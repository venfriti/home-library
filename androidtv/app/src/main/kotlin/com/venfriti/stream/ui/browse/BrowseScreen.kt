package com.venfriti.stream.ui.browse

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.GridItemSpan
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
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
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.ButtonDefaults
import androidx.tv.material3.Text
import coil.compose.AsyncImage
import com.venfriti.stream.data.ContentSection
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.ShowGroup
import com.venfriti.stream.network.ServerClient
import com.venfriti.stream.ui.theme.BgColor
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.GoldColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor

// One shared side margin for every element on the page -- toolbar, hero,
// section grids -- mirroring the website's own single --gutter variable
// (main.go) rather than each element picking its own inset. Applied once,
// as the page grid's own contentPadding, rather than by every section.
private val Gutter = 28.dp
private val GridSpacing = 14.dp
private const val PosterColumns = 6
// Wide cards (Continue Watching/Music Videos) span 2 of the 6 base
// columns, so 3 land per row -- the same "one grid, mixed span sizes"
// trick #file-grid/.cw-rail use on the website, see main.go.
private val WideCardSpan = GridItemSpan(2)

// Mirrors RebuildSections on the Windows client for section order
// (Continue Watching, Films, Series, Music Videos, Library), but the layout
// itself now follows the website's own #file-grid: a toolbar up top and a
// single grid of sections that wraps vertically -- exactly what a 10-foot
// D-pad UI already wants -- rather than the horizontally scrolling
// carousels this started as. One grid, not one-nested-grid-per-section:
// D-pad focus can't reliably jump between separate nested lazy layouts
// (each section's grid would need its own scroll/composition state to stay
// in sync with the page's), so every section lives in the same
// LazyVerticalGrid, with the hero and each rail head spanning the full
// width via GridItemSpan(maxLineSpan).
@Composable
fun BrowseScreen(client: ServerClient, onPlay: (FileEntry) -> Unit, onOpenShow: (ShowGroup) -> Unit) {
    var allFiles by remember { mutableStateOf<List<FileEntry>>(emptyList()) }
    var loading by remember { mutableStateOf(true) }

    LaunchedEffect(Unit) {
        // Mirrors the web's own default in main.go: `lsGet('stream-source')
        // || (HAS_LOCAL_LIB ? 'local' : 'cloud')` -- on the home network, the
        // local library is the primary source, with the R2-hosted one only a
        // fallback (unreachable local server, or nothing configured there).
        // /local/files is LAN-gated server-side, so a failure here is the
        // expected shape of "not on that network" rather than an error to
        // surface.
        val local = try {
            client.getLocalFiles()
        } catch (e: Exception) {
            emptyList()
        }
        val files = (local.ifEmpty { client.getFiles() }).filter { it.isPlayable }
        val progress = client.getAllProgress()
        files.forEach { f ->
            val p = progress[f.key]
            if (p != null && p.duration > 0) {
                f.progress = (p.position / p.duration).coerceIn(0.0, 1.0)
                f.positionSeconds = p.position
                f.durationSeconds = p.duration
                f.progressUpdated = p.updated
            }
        }
        allFiles = files
        loading = false
    }

    // Mirrors continueItems() in main.go's frontend JS / ContinueWatchingItems
    // on the Windows client: recency-sorted, music videos capped at 4, 8
    // total.
    val continueWatching = remember(allFiles) {
        val sorted = allFiles.filter { it.progress > 0 }.sortedByDescending { it.progressUpdated }
        var musicSeen = 0
        val result = mutableListOf<FileEntry>()
        for (f in sorted) {
            if (f.isMusicVideo) {
                musicSeen++
                if (musicSeen > 4) continue
            }
            result.add(f)
            if (result.size == 8) break
        }
        result
    }

    val films = remember(allFiles) { allFiles.filter { it.section == ContentSection.Film } }
    val shows = remember(allFiles) {
        allFiles.filter { it.section == ContentSection.Series }
            .groupBy { if (it.tmdbId > 0) it.tmdbId else -it.hashCode() }
            .map { (id, eps) -> ShowGroup(id, eps) }
            .sortedBy { it.displayTitle }
    }
    val musicVideos = remember(allFiles) { allFiles.filter { it.section == ContentSection.MusicVideo } }
    val library = remember(allFiles) { allFiles.filter { it.section == ContentSection.Library } }

    val hero = remember(continueWatching, allFiles) {
        continueWatching.firstOrNull { !it.heroArt.isNullOrEmpty() }
            ?: allFiles.firstOrNull { !it.heroArt.isNullOrEmpty() }
    }

    val heroFocusRequester = remember { FocusRequester() }
    // Same "nothing is focused until something asks for it" gap as
    // LoginScreen -- the hero's play/resume button is the natural first
    // stop for a remote once the wall has actually loaded.
    LaunchedEffect(hero) { if (hero != null) heroFocusRequester.requestFocus() }

    Column(modifier = Modifier.fillMaxSize().background(BgColor)) {
        TopBar()

        if (loading) {
            Box(modifier = Modifier.weight(1f).fillMaxSize(), contentAlignment = Alignment.Center) {
                Text(text = "Loading…", color = MutedColor)
            }
            return@Column
        }

        LazyVerticalGrid(
            columns = GridCells.Fixed(PosterColumns),
            modifier = Modifier.weight(1f).fillMaxWidth(),
            contentPadding = PaddingValues(start = Gutter, end = Gutter, bottom = 40.dp),
            horizontalArrangement = Arrangement.spacedBy(GridSpacing),
            verticalArrangement = Arrangement.spacedBy(GridSpacing),
        ) {
            fullSpan { HeroBanner(hero, onPlay, heroFocusRequester) }

            if (continueWatching.isNotEmpty()) {
                fullSpan { RailHead("Continue Watching") }
                items(continueWatching, span = { WideCardSpan }) { f ->
                    WideCard(
                        imageUrl = f.thumbnailImageUrl,
                        title = f.continueWatchingLine,
                        subtitle = f.remainingLabel,
                        progress = f.progress,
                        onClick = { onPlay(f) },
                    )
                }
            }

            if (films.isNotEmpty()) {
                fullSpan { RailHead("Films") }
                items(films) { f ->
                    PosterCard(
                        imageUrl = f.cardImageUrl,
                        title = f.title ?: f.name,
                        year = f.year.takeIf { it > 0 }?.toString(),
                        kind = "Film",
                        progress = f.progress,
                        onClick = { onPlay(f) },
                    )
                }
            }

            if (shows.isNotEmpty()) {
                fullSpan { RailHead("TV") }
                items(shows) { show ->
                    PosterCard(
                        imageUrl = show.cardImageUrl,
                        title = show.displayTitle,
                        year = show.year.takeIf { it > 0 }?.toString(),
                        extra = show.countLabel,
                        kind = "TV",
                        progress = show.progress,
                        onClick = { onOpenShow(show) },
                    )
                }
            }

            if (musicVideos.isNotEmpty()) {
                fullSpan { RailHead("Music Videos") }
                items(musicVideos, span = { WideCardSpan }) { f ->
                    WideCard(
                        imageUrl = f.thumbnailImageUrl,
                        title = f.displayTitle,
                        subtitle = f.artist,
                        progress = f.progress,
                        onClick = { onPlay(f) },
                    )
                }
            }

            if (library.isNotEmpty()) {
                fullSpan { RailHead("Library") }
                items(library) { f ->
                    PosterCard(
                        imageUrl = f.cardImageUrl,
                        title = f.title ?: f.name,
                        year = f.year.takeIf { it > 0 }?.toString(),
                        kind = "No artwork",
                        progress = f.progress,
                        onClick = { onPlay(f) },
                    )
                }
            }
        }
    }
}

private fun androidx.compose.foundation.lazy.grid.LazyGridScope.fullSpan(content: @Composable () -> Unit) {
    item(span = { GridItemSpan(maxLineSpan) }) { content() }
}

// Mirrors #topbar/header in main.go: just the wordmark on a TV, since the
// web's other header buttons (poster/list view, light/dark theme, qr,
// search) either don't apply here or aren't built yet.
@Composable
private fun TopBar() {
    Column(modifier = Modifier.fillMaxWidth()) {
        Row(
            verticalAlignment = Alignment.Bottom,
            modifier = Modifier.padding(horizontal = Gutter, vertical = 18.dp),
        ) {
            Text(text = "Stream", color = TextColor, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 18.sp)
            Text(text = "Videos", color = GoldColor, fontFamily = SerifFontFamily, fontStyle = FontStyle.Italic, fontSize = 18.sp)
        }
        Box(modifier = Modifier.fillMaxWidth().height(1.dp).background(BorderColor))
    }
}

// Mirrors .rail-head/.rh-t/.rh-l in main.go: a serif label baseline-aligned
// with a thin rule that fills the rest of the row, not a plain heading.
// Horizontal inset comes from the page grid's own contentPadding, not from
// here.
@Composable
private fun RailHead(label: String) {
    Row(
        verticalAlignment = Alignment.Bottom,
        modifier = Modifier.padding(top = 26.dp, bottom = 8.dp),
    ) {
        Text(
            text = label,
            color = TextColor,
            fontFamily = SerifFontFamily,
            fontWeight = FontWeight.Bold,
            fontSize = 15.sp,
        )
        Box(
            modifier = Modifier
                .padding(start = 11.dp, bottom = 7.dp)
                .weight(1f)
                .height(1.dp)
                .background(BorderColor),
        )
    }
}

// Mirrors renderHero() in main.go: a gold mono "kicker" line above the
// title -- "Resume · Xm left" when the pick is a part-watched title, else
// "Film/Series · year · ★rating" -- then a serif title, a 2-line synopsis,
// and a compact gold pill rather than a full-size button. Like .hero on the
// website, this is an inset rounded card (horizontal inset from the page
// grid's own contentPadding), not a full-bleed banner.
@Composable
private fun HeroBanner(hero: FileEntry?, onPlay: (FileEntry) -> Unit, focusRequester: FocusRequester) {
    if (hero == null) return
    val resuming = hero.progress > 0
    val kicker = if (resuming) {
        "Resume · ${hero.remainingLabel}"
    } else {
        listOfNotNull(
            if (hero.mediaType == "tv") "TV" else "Film",
            hero.year.takeIf { it > 0 }?.toString(),
            hero.rating.takeIf { it > 0 }?.let { "★ ${"%.1f".format(it)}" },
        ).joinToString(" · ")
    }
    val subtitle = if (hero.mediaType == "tv" && !hero.episodeTitle.isNullOrEmpty()) hero.episodeTitle else hero.overview

    Box(
        modifier = Modifier
            .fillMaxWidth()
            .padding(vertical = 18.dp)
            .height(320.dp)
            .clip(RoundedCornerShape(8.dp)),
    ) {
        AsyncImage(
            model = hero.heroArt,
            contentDescription = hero.displayTitle,
            contentScale = ContentScale.Crop,
            // Crop biased toward the top of the image (mirrors the web's
            // own object-position:center 22%) rather than a dead-center
            // crop, since what's worth keeping in a backdrop -- faces, a
            // title treatment -- usually sits in the upper half.
            alignment = Alignment.TopCenter,
            modifier = Modifier.fillMaxSize(),
        )
        // Mirrors .hero-scrim's two stacked gradients: a left-heavy
        // horizontal one so the caption reads over any art, plus a
        // bottom vertical one tying the banner into the rail below it.
        Box(
            modifier = Modifier
                .fillMaxSize()
                .background(
                    Brush.horizontalGradient(
                        0.0f to BgColor.copy(alpha = 0.95f),
                        0.42f to BgColor.copy(alpha = 0.72f),
                        0.82f to BgColor.copy(alpha = 0.15f),
                    ),
                ),
        )
        Box(
            modifier = Modifier
                .fillMaxSize()
                .background(Brush.verticalGradient(0.0f to Color.Transparent, 0.45f to Color.Transparent, 1.0f to BgColor.copy(alpha = 0.9f))),
        )
        Column(
            modifier = Modifier.align(Alignment.BottomStart).padding(28.dp).fillMaxWidth(0.6f),
        ) {
            Text(
                text = kicker,
                color = GoldColor,
                fontFamily = MonoFontFamily,
                fontSize = 10.sp,
                letterSpacing = 1.5.sp,
                modifier = Modifier.padding(bottom = 6.dp),
            )
            Text(
                text = hero.displayTitle,
                color = Color.White,
                fontFamily = SerifFontFamily,
                fontSize = 22.sp,
                lineHeight = 26.sp,
                fontWeight = FontWeight.Bold,
            )
            if (!subtitle.isNullOrEmpty()) {
                Text(
                    text = subtitle,
                    color = Color(0xFFCFC9BF),
                    fontSize = 12.sp,
                    lineHeight = 17.sp,
                    maxLines = 2,
                    modifier = Modifier.padding(top = 8.dp, bottom = 12.dp),
                )
            }
            androidx.tv.material3.Button(
                onClick = { onPlay(hero) },
                modifier = Modifier.focusRequester(focusRequester),
                shape = ButtonDefaults.shape(
                    RoundedCornerShape(3.dp),
                    RoundedCornerShape(3.dp),
                    RoundedCornerShape(3.dp),
                    RoundedCornerShape(3.dp),
                    RoundedCornerShape(3.dp),
                ),
                colors = ButtonDefaults.colors(
                    containerColor = GoldColor,
                    contentColor = Color(0xFF0A0A0A),
                    focusedContainerColor = GoldColor,
                    focusedContentColor = Color(0xFF0A0A0A),
                ),
            ) {
                Text(
                    text = "▶  " + if (resuming) "RESUME" else "PLAY",
                    fontFamily = MonoFontFamily,
                    letterSpacing = 1.sp,
                    fontSize = 12.sp,
                )
            }
        }
    }
}
