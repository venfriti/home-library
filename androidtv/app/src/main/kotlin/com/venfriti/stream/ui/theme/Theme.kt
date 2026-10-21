package com.venfriti.stream.ui.theme

import androidx.compose.runtime.Composable
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.Font
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.Typography
import androidx.tv.material3.darkColorScheme
import com.venfriti.stream.R

// ---- Palette ---------------------------------------------------------------
// Same tokens as the site's :root (main.go) / the Windows client's App.xaml,
// plus the few extra steps a 10-foot UI needs: a lighter gold for anything
// drawn over moving video, a readable muted grey (the web's #666 fails
// contrast on a TV across the room), and an "inverse" fill for focused rows.
val BgColor = Color(0xFF0A0A0A)
val Bg2Color = Color(0xFF141414)
val Bg3Color = Color(0xFF1E1E1E)
val SheetColor = Color(0xFF151818)      // drawer, dialogs, side sheets
val BorderColor = Color(0xFF2A2A2A)
val GoldColor = Color(0xFF9A7D4A)       // filled buttons on the wall
val GoldTextColor = Color(0xFFC9A765)   // kickers, player accent
val TextColor = Color(0xFFECE8E1)
val TextDimColor = Color(0xFFD6D0C6)
val MutedColor = Color(0xFF9A958C)
val InkColor = Color(0xFF0A0A0A)        // text on gold / on the inverse fill
val TonalColor = Color(0x24ECE8E1)      // Material "tonal" button fill, ~14% text
val HeroTint = Color(0xFF14252B)        // the site's teal wash behind the header

// ---- Type ------------------------------------------------------------------
// The web's three font roles, bundled in res/font so the first frame renders
// offline (they were Google Fonts over the network on the site).
// --font-sans is stood in for by DM Sans; swap the three dm_sans_* files if
// the site uses something else.
val SerifFontFamily = FontFamily(
    Font(R.font.libre_baskerville_regular, FontWeight.Normal),
    Font(R.font.libre_baskerville_bold, FontWeight.Bold),
    Font(R.font.libre_baskerville_italic, FontWeight.Normal, FontStyle.Italic),
)
val MonoFontFamily = FontFamily(
    Font(R.font.jetbrains_mono_regular, FontWeight.Normal),
    Font(R.font.jetbrains_mono_medium, FontWeight.Medium),
)
val SansFontFamily = FontFamily(
    Font(R.font.dm_sans_regular, FontWeight.Normal),
    Font(R.font.dm_sans_medium, FontWeight.Medium),
    Font(R.font.dm_sans_semibold, FontWeight.SemiBold),
)

// ---- Layout ----------------------------------------------------------------
// Android TV's canvas is 960x540dp. 48dp side margins keep everything inside
// the overscan-safe area; content starts after the 60dp nav rail.
object Dimens {
    val RailWidth = 60.dp
    val DrawerWidth = 270.dp
    val ContentStart = 84.dp
    val Gutter = 48.dp
    val WideCardWidth = 210.dp
    val PosterWidth = 120.dp
    val RowGap = 20.dp
}

private val StreamColorScheme = darkColorScheme(
    primary = GoldColor,
    onPrimary = InkColor,
    secondary = GoldTextColor,
    background = BgColor,
    onBackground = TextColor,
    surface = Bg2Color,
    onSurface = TextColor,
    surfaceVariant = Bg3Color,
    border = BorderColor,
)

private val sans = TextStyle(fontFamily = SansFontFamily)

// Only the family is set here -- every Text in the app states its own size,
// sized for a 10-foot screen rather than a browser window.
private val StreamTypography = Typography(
    bodyLarge = sans,
    bodyMedium = sans,
    bodySmall = sans,
    labelLarge = sans,
    labelMedium = sans,
    labelSmall = sans,
    titleLarge = sans,
    titleMedium = sans,
    titleSmall = sans,
)

@Composable
fun StreamTheme(content: @Composable () -> Unit) {
    MaterialTheme(
        colorScheme = StreamColorScheme,
        typography = StreamTypography,
        content = content,
    )
}
