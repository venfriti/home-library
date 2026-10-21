package com.venfriti.stream.ui.browse

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.BringIntoViewSpec
import androidx.compose.ui.draw.drawWithContent
import androidx.compose.ui.graphics.BlendMode
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.CompositingStrategy
import androidx.compose.ui.graphics.graphicsLayer
import com.venfriti.stream.ui.common.Artwork
import androidx.compose.foundation.gestures.LocalBringIntoViewSpec
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.platform.LocalConfiguration
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.Library
import com.venfriti.stream.data.ShowGroup
import com.venfriti.stream.ui.common.ButtonKind
import com.venfriti.stream.ui.common.RestoreFocus
import com.venfriti.stream.ui.common.StreamButton
import com.venfriti.stream.ui.common.StreamIcons
import kotlinx.coroutines.launch
import com.venfriti.stream.ui.common.rememberFocusRestore
import com.venfriti.stream.ui.common.restorable
import com.venfriti.stream.ui.nav.Section
import com.venfriti.stream.ui.theme.Dimens
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextDimColor
import androidx.compose.ui.graphics.Color

private const val RailCap = 15

// A title as the hero presents it. The hero itself is fixed to the most
// recent part-watched title that has art; a focused tile's Featured only
// feeds the page's colour tint.
data class Featured(
    val key: String,
    val art: String?,
    val kicker: String,
    val title: String,
    val subtitle: String?,
    val entry: FileEntry? = null,
    val show: ShowGroup? = null,
)

fun featuredFor(f: FileEntry): Featured {
    val kicker = if (f.progress > 0 && f.remainingLabel.isNotEmpty()) {
        "Resume · ${f.remainingLabel}"
    } else {
        listOfNotNull(
            when {
                f.isMusicVideo -> "Music video"
                f.mediaType == "tv" -> "TV"
                f.mediaType == "movie" -> "Film"
                else -> null
            },
            f.year.takeIf { it > 0 }?.toString(),
            f.rating.takeIf { it > 0 }?.let { "★ ${"%.1f".format(it)}" },
        ).joinToString(" · ")
    }
    val subtitle = when {
        f.mediaType == "tv" && f.season > 0 && f.episode > 0 ->
            "S${"%02d".format(f.season)}E${"%02d".format(f.episode)}" + (f.episodeTitle?.let { " · $it" } ?: "")
        f.isMusicVideo -> f.artist
        else -> f.overview
    }
    return Featured(
        key = f.key,
        art = f.heroArt ?: f.thumbnailImageUrl,
        kicker = kicker.uppercase(),
        title = f.title ?: f.name,
        subtitle = subtitle,
        entry = f,
    )
}

fun featuredFor(s: ShowGroup): Featured {
    val first = s.episodes.first()
    return Featured(
        key = "show:${s.tmdbId}",
        art = first.backdrop ?: first.episodeStill ?: s.posterUrl,
        kicker = "TV · ${s.countLabel.uppercase()}",
        title = s.displayTitle,
        subtitle = first.overview,
        show = s,
    )
}

@OptIn(androidx.compose.foundation.ExperimentalFoundationApi::class)
@Composable
fun HomeScreen(
    library: Library,
    onPlay: (FileEntry) -> Unit,
    // Continue watching and the hero (its first item): resume in the list
    // the title was originally played from.
    onResume: (FileEntry) -> Unit = onPlay,
    onOpenShow: (ShowGroup) -> Unit,
    onOpenMovie: (FileEntry) -> Unit,
    onOpenSection: (Section) -> Unit,
    onLongPress: (FileEntry) -> Unit,
) {
    val continueWatching = library.continueWatching
    val films = library.films
    val shows = library.shows
    val music = library.musicVideos
    val other = library.other

    val initial = remember(library.files) {
        (continueWatching.firstOrNull { it.heroArt != null } ?: library.files.firstOrNull { it.heroArt != null })
            ?.let(::featuredFor)
    }
    // The hero is fixed to the featured title; a focused card only changes
    // the page's colour tint (`focused`), never the hero or its picture.
    var featured by remember { mutableStateOf<Featured?>(null) }
    val hero = initial
    val focused = featured ?: initial

    val restore = rememberFocusRestore()
    // First focus: Resume. The top area never scrolls (see bringIntoView
    // below), so focusing it no longer nudges the page.
    val heroButton = remember { FocusRequester() }
    RestoreFocus(restore, ready = !library.loading && hero != null, fallback = heroButton)

    fun showFor(f: FileEntry): ShowGroup? = library.show(Library.showId(f))

    val listState = rememberLazyListState()
    val scope = rememberCoroutineScope()
    val heroHeight = (LocalConfiguration.current.screenHeightDp * 0.68f).dp

    // Focus in the top area (Home icon, Resume, Episodes): the page stays put.
    // Below it (Continue watching onward): normal TV scrolling. Starts false
    // and only flips when focus actually enters the hero -- coming back from
    // a film page lands focus on a card, and a `true` default was never
    // corrected in that case.
    var heroFocused by remember { mutableStateOf(false) }
    val baseSpec = LocalBringIntoViewSpec.current
    val bringIntoView = remember(baseSpec) {
        object : BringIntoViewSpec {
            override val scrollAnimationSpec get() = baseSpec.scrollAnimationSpec
            // On TV the default keeps the focused item ~30% down the screen,
            // which nudged the page as soon as Resume took focus.
            override fun calculateScrollDistance(offset: Float, size: Float, containerSize: Float): Float =
                if (heroFocused) 0f else baseSpec.calculateScrollDistance(offset, size, containerSize)
        }
    }

    Box(modifier = Modifier.fillMaxSize()) {
        // Colour only. The one picture on Home lives inside the hero item.
        TintBackdrop(art = focused?.art)

        if (library.loading) {
            Text("Loading…", color = MutedColor, fontSize = 16.sp, modifier = Modifier.align(Alignment.Center))
            return@Box
        }

        CompositionLocalProvider(LocalBringIntoViewSpec provides bringIntoView) {
        LazyColumn(
            state = listState,
            contentPadding = PaddingValues(bottom = 48.dp),
            verticalArrangement = Arrangement.spacedBy(22.dp),
            modifier = Modifier.fillMaxSize(),
        ) {
            item(key = "hero") {
              // ~68% of the screen, like the show page's backdrop before its
              // episode row, with the title and buttons at the bottom of it.
              Box(modifier = Modifier.fillMaxWidth().height(heroHeight)) {
                // The hero's own picture, part of this item: it scrolls away
                // with the hero and fades into the page, so it can never sit
                // behind the rows or follow card focus.
                // A band of the hero's own tint colour behind the bottom of
                // its picture, so the glow where the picture fades shows with
                // the page at the top too (the page tint is strongest at the
                // top of the screen, which the picture covers until you
                // scroll).
                val heroAccent = rememberAccentColor(hero?.art)
                Box(
                    modifier = Modifier.matchParentSize().background(
                        Brush.verticalGradient(
                            0.45f to Color.Transparent,
                            0.82f to heroAccent,
                            1f to Color.Transparent,
                        ),
                    ),
                )
                HeroArt(art = hero?.art, modifier = Modifier.matchParentSize())
                Column(
                    modifier = Modifier.align(Alignment.BottomStart).fillMaxWidth().onFocusChanged {
                        if (it.hasFocus != heroFocused) {
                            heroFocused = it.hasFocus
                            // Coming back up from the rows: return to the top
                            // and to the hero's own tint.
                            if (it.hasFocus) {
                                featured = null
                                scope.launch { listState.animateScrollToItem(0) }
                            }
                        }
                    },
                ) {
                    if (hero != null) HeroBlock(hero, heroButton, onResume, onOpenShow, ::showFor)
                }
              }
            }

            item(key = "cw") {
                Rail("Continue watching", continueWatching.size, continueWatching, key = { it.key }) { f ->
                    WideCard(
                        imageUrl = f.thumbnailImageUrl,
                        title = f.continueWatchingLine,
                        subtitle = f.remainingLabel,
                        progress = f.progress,
                        width = Dimens.WideCardWidth,
                        modifier = Modifier.restorable(restore, "cw:${f.key}", fileKey = f.key),
                        // Continue watching keeps the hero's tint; it first
                        // changes once focus goes below this row.
                        onFocus = { if (it) featured = null },
                        onLongClick = { onLongPress(f) },
                        onClick = { onResume(f) },
                    )
                }
            }

            item(key = "films") {
                Rail(
                    "Films", films.size, films.take(RailCap), key = { it.key },
                    onSeeAll = if (films.size > RailCap) { { onOpenSection(Section.Films) } } else null,
                ) { f ->
                    PosterCard(
                        imageUrl = f.cardImageUrl,
                        title = f.title ?: f.name,
                        year = f.year.takeIf { it > 0 }?.toString(),
                        kind = "Film",
                        progress = f.progress,
                        width = Dimens.PosterWidth,
                        modifier = Modifier.restorable(restore, "film:${f.key}"),
                        onFocus = { if (it) featured = featuredFor(f) },
                        onLongClick = { onLongPress(f) },
                        onClick = { onOpenMovie(f) },
                    )
                }
            }

            item(key = "series") {
                Rail(
                    "TV", shows.size, shows.take(RailCap), key = { it.tmdbId.toString() },
                    onSeeAll = if (shows.size > RailCap) { { onOpenSection(Section.Series) } } else null,
                ) { s ->
                    PosterCard(
                        imageUrl = s.cardImageUrl,
                        title = s.displayTitle,
                        year = s.year.takeIf { it > 0 }?.toString(),
                        extra = s.countLabel,
                        kind = "TV",
                        progress = s.progress,
                        width = Dimens.PosterWidth,
                        modifier = Modifier.restorable(restore, "show:${s.tmdbId}"),
                        onFocus = { if (it) featured = featuredFor(s) },
                        onClick = { onOpenShow(s) },
                    )
                }
            }

            item(key = "playlists") {
                Rail("Playlists", library.playlists.size, library.playlists, key = { it.id }) { p ->
                    val items = library.playlistItems(p)
                    WideCard(
                        imageUrl = items.firstOrNull()?.thumbnailImageUrl,
                        title = p.name,
                        subtitle = "${items.size} ${if (items.size == 1) "item" else "items"}",
                        width = Dimens.WideCardWidth,
                        modifier = Modifier.restorable(restore, "pl:${p.id}"),
                        onFocus = { if (it) items.firstOrNull()?.let { f -> featured = featuredFor(f).copy(key = "pl:${p.id}", title = p.name, kicker = "PLAYLIST", subtitle = null, entry = null) } },
                        onClick = { onOpenSection(Section.Playlists) },
                    )
                }
            }

            item(key = "music") {
                Rail(
                    "Music videos", music.size, music.take(RailCap), key = { it.key },
                    onSeeAll = if (music.size > RailCap) { { onOpenSection(Section.Music) } } else null, seeAllWide = true,
                ) { f ->
                    WideCard(
                        imageUrl = f.thumbnailImageUrl,
                        title = f.title ?: f.name,
                        subtitle = f.artist,
                        // No progress sliver: music videos always start from
                        // the beginning, so it says nothing useful.
                        progress = 0.0,
                        width = Dimens.WideCardWidth,
                        modifier = Modifier.restorable(restore, "mv:${f.key}", fileKey = f.key),
                        onFocus = { if (it) featured = featuredFor(f) },
                        onLongClick = { onLongPress(f) },
                        onClick = { onPlay(f) },
                    )
                }
            }

            item(key = "library") {
                Rail(
                    "Library", other.size, other.take(RailCap), key = { it.key },
                    onSeeAll = if (other.size > RailCap) { { onOpenSection(Section.Library) } } else null,
                ) { f ->
                    PosterCard(
                        imageUrl = f.cardImageUrl,
                        title = f.title ?: f.name,
                        kind = "No artwork",
                        progress = f.progress,
                        width = Dimens.PosterWidth,
                        modifier = Modifier.restorable(restore, "lib:${f.key}"),
                        onFocus = { if (it) featured = featuredFor(f) },
                        onLongClick = { onLongPress(f) },
                        onClick = { onPlay(f) },
                    )
                }
            }
        }
        }
    }
}

// The featured title's backdrop, confined to the hero item and masked so it
// fades out towards the text on the left and into the page at the bottom
// (showing the colour tint through, rather than painting a dark scrim).
@Composable
private fun HeroArt(art: String?, modifier: Modifier = Modifier) {
    if (art == null) return
    Artwork(
        url = art,
        description = null,
        alignment = Alignment.TopCenter,
        modifier = modifier
            .graphicsLayer { compositingStrategy = CompositingStrategy.Offscreen }
            .drawWithContent {
                drawContent()
                drawRect(
                    Brush.horizontalGradient(0f to Color.Transparent, 0.36f to Color.Black.copy(alpha = 0.28f), 0.68f to Color.Black),
                    blendMode = BlendMode.DstIn,
                )
                drawRect(
                    Brush.verticalGradient(0f to Color.Black, 0.6f to Color.Black, 1f to Color.Transparent),
                    blendMode = BlendMode.DstIn,
                )
            },
    )
}

@Composable
private fun HeroBlock(
    hero: Featured,
    primaryRequester: FocusRequester,
    onPlay: (FileEntry) -> Unit,
    onOpenShow: (ShowGroup) -> Unit,
    showFor: (FileEntry) -> ShowGroup?,
) {
    val entry = hero.entry
    val episodeShow = entry?.takeIf { it.mediaType == "tv" }?.let(showFor)
    Column(
        modifier = Modifier
            .padding(start = Dimens.ContentStart, bottom = 20.dp)
            .fillMaxWidth(0.5f),
        verticalArrangement = Arrangement.spacedBy(9.dp),
    ) {
        if (hero.kicker.isNotEmpty()) {
            Text(hero.kicker, color = GoldTextColor, fontFamily = MonoFontFamily, fontSize = 12.sp, letterSpacing = 2.sp)
        }
        Text(
            text = hero.title,
            color = Color.White,
            fontFamily = SerifFontFamily,
            fontWeight = FontWeight.Bold,
            fontSize = 48.sp,
            lineHeight = 52.sp,
            maxLines = 2,
            overflow = TextOverflow.Ellipsis,
        )
        if (!hero.subtitle.isNullOrEmpty()) {
            Text(
                text = hero.subtitle,
                color = TextDimColor,
                fontSize = 17.sp,
                lineHeight = 24.sp,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
            )
        }
        Row(horizontalArrangement = Arrangement.spacedBy(10.dp), modifier = Modifier.padding(top = 8.dp)) {
            val resuming = (entry?.progress ?: 0.0) > 0
            StreamButton(
                label = when {
                    hero.show != null -> "Episodes"
                    resuming -> "Resume"
                    else -> "Play"
                },
                icon = if (hero.show != null) null else StreamIcons.Play,
                kind = ButtonKind.Primary,
                modifier = Modifier.focusRequester(primaryRequester),
                onClick = {
                    when {
                        hero.show != null -> onOpenShow(hero.show)
                        entry != null -> onPlay(entry)
                    }
                },
            )
            if (episodeShow != null) {
                StreamButton(label = "Episodes", onClick = { onOpenShow(episodeShow) })
            }
        }
    }
}
