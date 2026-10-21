package com.venfriti.stream.ui.browse

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.ui.common.Artwork
import com.venfriti.stream.ui.common.FocusSurface
import com.venfriti.stream.ui.common.ProgressBar
import com.venfriti.stream.ui.theme.Bg2Color
import com.venfriti.stream.ui.theme.Bg3Color
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TextDimColor

// Poster wall -- a gallery, not a data grid: the cover does the work at rest
// and the caption only appears on focus (posterOverlay()/.pc-ovl in main.go).
@Composable
fun PosterCard(
    imageUrl: String?,
    title: String,
    modifier: Modifier = Modifier,
    width: Dp? = null,
    year: String? = null,
    extra: String? = null,
    kind: String? = null,
    progress: Double = 0.0,
    onFocus: (Boolean) -> Unit = {},
    onLongClick: (() -> Unit)? = null,
    onClick: () -> Unit,
) {
    val shape = RoundedCornerShape(6.dp)
    FocusSurface(
        onClick = onClick,
        onLongClick = onLongClick,
        onFocus = onFocus,
        modifier = modifier
            .let { if (width != null) it.width(width) else it.fillMaxWidth() }
            .aspectRatio(2f / 3f),
        shape = shape,
        containerColor = Bg2Color,
        focusedScale = 1.08f,
        contentModifier = Modifier.fillMaxSize(),
    ) { focused ->
        if (imageUrl != null) {
            Artwork(url = imageUrl, description = title, modifier = Modifier.fillMaxSize())
        } else {
            NoArtCard(title = title, kind = kind)
        }

        val line = listOfNotNull(year, extra).joinToString("  ·  ")
        if (focused && imageUrl != null) {
            Box(
                modifier = Modifier
                    .fillMaxSize()
                    .background(
                        Brush.verticalGradient(
                            0.0f to Color.Transparent,
                            0.40f to Color.Transparent,
                            0.68f to Color.Black.copy(alpha = 0.5f),
                            1.0f to Color.Black.copy(alpha = 0.92f),
                        ),
                    ),
            )
            Column(modifier = Modifier.align(Alignment.BottomStart).padding(10.dp)) {
                Text(
                    text = title,
                    color = Color.White,
                    fontSize = 13.sp,
                    lineHeight = 16.sp,
                    fontWeight = FontWeight.Medium,
                    maxLines = 3,
                    overflow = TextOverflow.Ellipsis,
                )
                if (line.isNotEmpty()) {
                    Text(
                        text = line,
                        color = GoldTextColor,
                        fontFamily = MonoFontFamily,
                        fontSize = 10.sp,
                        modifier = Modifier.padding(top = 3.dp),
                    )
                }
            }
        }

        ProgressBar(progress = progress, modifier = Modifier.align(Alignment.BottomStart))
    }
}

// Stand-in for titles TMDB never matched: a set title card in the serif,
// not a broken-image box (noArtCard()/.pc-noart).
@Composable
fun NoArtCard(title: String, kind: String?) {
    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Brush.linearGradient(listOf(Bg3Color, Bg2Color))),
    ) {
        Column(modifier = Modifier.align(Alignment.BottomStart).padding(12.dp)) {
            Text(
                text = title,
                color = TextColor,
                fontFamily = SerifFontFamily,
                fontWeight = FontWeight.Bold,
                fontSize = 14.sp,
                lineHeight = 18.sp,
                maxLines = 4,
                overflow = TextOverflow.Ellipsis,
            )
            Text(
                text = (kind ?: "No artwork").uppercase(),
                color = MutedColor,
                fontFamily = MonoFontFamily,
                fontSize = 9.sp,
                letterSpacing = 1.sp,
                modifier = Modifier.padding(top = 8.dp),
            )
        }
    }
}

// Continue watching / music videos / episodes: 16:9 stills with a caption
// that's always shown (a frame grab identifies nothing on its own).
@Composable
fun WideCard(
    imageUrl: String?,
    title: String,
    subtitle: String?,
    modifier: Modifier = Modifier,
    width: Dp? = null,
    badge: String? = null,
    progress: Double = 0.0,
    onFocus: (Boolean) -> Unit = {},
    onLongClick: (() -> Unit)? = null,
    onClick: () -> Unit,
) {
    var focused by remember { mutableStateOf(false) }
    val shape = RoundedCornerShape(6.dp)
    Column(modifier = modifier.let { if (width != null) it.width(width) else it.fillMaxWidth() }) {
        FocusSurface(
            onClick = onClick,
            onLongClick = onLongClick,
            onFocus = { focused = it; onFocus(it) },
            modifier = Modifier.fillMaxWidth().aspectRatio(16f / 9f),
            shape = shape,
            containerColor = Bg2Color,
            focusedScale = 1.08f,
            contentModifier = Modifier.fillMaxSize(),
        ) {
            if (imageUrl != null) {
                Artwork(url = imageUrl, description = title, modifier = Modifier.fillMaxSize())
            } else {
                NoArtCard(title = title, kind = null)
            }
            if (badge != null) {
                Text(
                    text = badge,
                    color = Color.White.copy(alpha = 0.9f),
                    fontFamily = MonoFontFamily,
                    fontSize = 10.sp,
                    letterSpacing = 1.sp,
                    modifier = Modifier.align(Alignment.TopStart).padding(8.dp),
                )
            }
            ProgressBar(progress = progress, modifier = Modifier.align(Alignment.BottomStart))
        }
        Text(
            text = title,
            color = if (focused) Color.White else TextDimColor,
            fontSize = 14.sp,
            lineHeight = 18.sp,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            // Extra top room so the 1.08x lift never overlaps the caption.
            modifier = Modifier.padding(top = 12.dp),
        )
        if (!subtitle.isNullOrEmpty()) {
            Text(
                text = subtitle,
                color = MutedColor,
                fontFamily = MonoFontFamily,
                fontSize = 11.sp,
                maxLines = 1,
                modifier = Modifier.padding(top = 3.dp),
            )
        }
    }
}
