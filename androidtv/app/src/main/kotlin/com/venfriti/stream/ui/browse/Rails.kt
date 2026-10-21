package com.venfriti.stream.ui.browse

import androidx.compose.animation.Crossfade
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.compositionLocalOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.graphics.ColorUtils
import androidx.palette.graphics.Palette
import androidx.tv.material3.Text
import coil.Coil
import coil.request.ImageRequest
import com.venfriti.stream.ui.common.Artwork
import com.venfriti.stream.ui.common.FocusSurface
import com.venfriti.stream.ui.theme.BgColor
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.Dimens
import com.venfriti.stream.ui.theme.HeroTint
import com.venfriti.stream.ui.theme.GoldColor
import com.venfriti.stream.ui.theme.InkColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TonalColor

// Extra room at the end of each rail while the music mini player sits in
// the bottom-right corner, so the last tiles can scroll out from under it.
val LocalMiniPlayerInset = compositionLocalOf { 0.dp }

// "Stream" + gold italic "Videos", as in the site header.
@Composable
fun Wordmark(modifier: Modifier = Modifier) {
    Row(modifier = modifier, verticalAlignment = Alignment.Bottom) {
        Text("Stream", color = TextColor, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 19.sp)
        Text("Videos", color = GoldColor, fontFamily = SerifFontFamily, fontStyle = FontStyle.Italic, fontSize = 19.sp)
    }
}

// .rail-head: serif label, mono count, thin rule to the edge.
@Composable
fun RailHead(label: String, count: Int? = null, modifier: Modifier = Modifier) {
    Row(
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
        modifier = modifier.fillMaxWidth().padding(start = Dimens.ContentStart, end = Dimens.Gutter),
    ) {
        Text(label, color = TextColor, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 20.sp)
        if (count != null) Text(count.toString(), color = MutedColor, fontFamily = MonoFontFamily, fontSize = 11.sp)
        Box(modifier = Modifier.weight(1f).padding(start = 4.dp).height(1.dp).background(BorderColor))
    }
}

// One horizontally scrolling row. focusRestorer: moving up/down between rails
// comes back to the tile you last had in each row, not the first one.
@OptIn(ExperimentalComposeUiApi::class)
@Composable
fun <T> Rail(
    title: String,
    count: Int?,
    items: List<T>,
    key: (T) -> String,
    modifier: Modifier = Modifier,
    onSeeAll: (() -> Unit)? = null,
    seeAllWide: Boolean = false,
    itemContent: @Composable (T) -> Unit,
) {
    if (items.isEmpty()) return
    Column(modifier = modifier.fillMaxWidth()) {
        RailHead(title, count)
        LazyRow(
            state = rememberLazyListState(),
            contentPadding = PaddingValues(
                start = Dimens.ContentStart,
                end = Dimens.Gutter + LocalMiniPlayerInset.current,
                top = 16.dp,
                bottom = 12.dp,
            ),
            horizontalArrangement = Arrangement.spacedBy(Dimens.RowGap),
            modifier = Modifier.fillMaxWidth().focusRestorer(),
        ) {
            itemsIndexed(items, key = { i, t -> "$i:${key(t)}" }) { _, t -> itemContent(t) }
            if (onSeeAll != null) {
                item(key = "__see_all") {
                    SeeAllTile(
                        label = "See all ${count ?: items.size}",
                        width = if (seeAllWide) Dimens.WideCardWidth else Dimens.PosterWidth,
                        aspect = if (seeAllWide) 16f / 9f else 2f / 3f,
                        onClick = onSeeAll,
                    )
                }
            }
        }
    }
}

@Composable
fun SeeAllTile(label: String, width: Dp, aspect: Float, onClick: () -> Unit) {
    FocusSurface(
        onClick = onClick,
        modifier = Modifier.width(width).aspectRatio(aspect),
        shape = RoundedCornerShape(6.dp),
        containerColor = TonalColor,
        focusedContainerColor = TextColor,
        focusedScale = 1.08f,
        contentModifier = Modifier.fillMaxSize(),
    ) { focused ->
        Text(
            text = label.uppercase(),
            color = if (focused) InkColor else TextColor,
            fontFamily = MonoFontFamily,
            fontSize = 11.sp,
            letterSpacing = 1.5.sp,
            modifier = Modifier.align(Alignment.Center),
        )
    }
}

// Full-bleed backdrop that crossfades to whatever is focused, under the two
// scrims from .hero-scrim (left-heavy for the caption, bottom fade into the
// page).
@Composable
fun Backdrop(art: String?, modifier: Modifier = Modifier, heightFraction: Float = 0.8f) {
    Box(modifier = modifier.fillMaxSize().background(BgColor)) {
        Crossfade(targetState = art, animationSpec = tween(450), label = "backdrop") { url ->
            Box(modifier = Modifier.fillMaxWidth().fillMaxHeight(heightFraction)) {
                // Crop biased to the upper part of the frame (object-position
                // center 22% on the web): faces and title art live up there.
                Artwork(url = url, description = null, modifier = Modifier.fillMaxSize(), alignment = Alignment.TopCenter)
            }
        }
        Box(
            modifier = Modifier.fillMaxSize().background(
                Brush.horizontalGradient(
                    0.0f to BgColor.copy(alpha = 0.96f),
                    0.36f to BgColor.copy(alpha = 0.72f),
                    0.68f to Color.Transparent,
                ),
            ),
        )
        Box(
            modifier = Modifier.fillMaxSize().background(
                Brush.verticalGradient(
                    0.0f to Color.Transparent,
                    0.38f to Color.Transparent,
                    0.62f to BgColor.copy(alpha = 0.9f),
                    0.76f to BgColor,
                ),
            ),
        )
    }
}

// Same shape as Backdrop (a glow at the top fading into the page) but never
// paints the photo itself -- just a Palette-sampled accent colour, the way
// the web's ambient-tint system reads a poster's dominant colour rather than
// splashing the poster across the whole wall on every card focus.
@Composable
fun TintBackdrop(art: String?, modifier: Modifier = Modifier, heightFraction: Float = 0.8f) {
    val animated = rememberAccentColor(art)

    Box(modifier = modifier.fillMaxSize().background(BgColor)) {
        Box(
            modifier = Modifier.fillMaxWidth().fillMaxHeight(heightFraction).background(
                Brush.verticalGradient(0.0f to animated, 1.0f to Color.Transparent),
            ),
        )
        Box(
            modifier = Modifier.fillMaxSize().background(
                Brush.horizontalGradient(
                    0.0f to BgColor.copy(alpha = 0.96f),
                    0.36f to BgColor.copy(alpha = 0.72f),
                    0.68f to Color.Transparent,
                ),
            ),
        )
        Box(
            modifier = Modifier.fillMaxSize().background(
                Brush.verticalGradient(
                    0.0f to Color.Transparent,
                    0.38f to Color.Transparent,
                    0.62f to BgColor.copy(alpha = 0.9f),
                    0.76f to BgColor,
                ),
            ),
        )
    }
}

// The ambient accent for a piece of art (see extractAccentColor), animated
// between changes. Shared by the page tint and the hero's own band so the
// two are always exactly the same colour.
@Composable
fun rememberAccentColor(art: String?): Color {
    val context = LocalContext.current
    var accent by remember { mutableStateOf(HeroTint) }
    LaunchedEffect(art) {
        accent = extractAccentColor(context, art) ?: HeroTint
    }
    val animated by animateColorAsState(accent, animationSpec = tween(450), label = "accentTint")
    return animated
}

// Downsamples the art to a software bitmap (Palette can't read hardware
// bitmaps) and picks a swatch, clamped to a saturation/lightness band that
// stays legible as a full-screen wash behind white text -- mirrors the web's
// normaliseAccent() in main.go, tuned darker since this sits behind a whole
// page instead of a small accent chip.
private suspend fun extractAccentColor(context: android.content.Context, url: String?): Color? {
    if (url.isNullOrEmpty()) return null
    return try {
        val request = ImageRequest.Builder(context).data(url).allowHardware(false).size(64, 64).build()
        val result = Coil.imageLoader(context).execute(request)
        val bitmap = (result.drawable as? android.graphics.drawable.BitmapDrawable)?.bitmap ?: return null
        val palette = Palette.from(bitmap).generate()
        val swatch = palette.vibrantSwatch ?: palette.dominantSwatch ?: palette.mutedSwatch ?: return null
        val hsl = FloatArray(3)
        ColorUtils.colorToHSL(swatch.rgb, hsl)
        hsl[1] = hsl[1].coerceIn(0.35f, 0.65f)
        hsl[2] = 0.16f
        Color(ColorUtils.HSLToColor(hsl))
    } catch (e: Exception) {
        null
    }
}
