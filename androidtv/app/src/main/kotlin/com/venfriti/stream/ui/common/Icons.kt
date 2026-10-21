package com.venfriti.stream.ui.common

import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.size
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.ColorFilter
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.graphics.vector.addPathNodes
import androidx.compose.ui.graphics.vector.rememberVectorPainter
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

// Material Symbols-style outline icons, drawn from the same 24x24 paths as the
// design canvas. Kept in-app (not material-icons-extended) because that
// artifact adds several MB of dex for the ~15 glyphs used here, and the
// transport bar used to rely on unicode glyphs (❚❚ ◀◀) that render
// differently on every TV box.
private fun strokeIcon(name: String, vararg paths: String): ImageVector =
    ImageVector.Builder(name, 24.dp, 24.dp, 24f, 24f).apply {
        paths.forEach { d ->
            addPath(
                pathData = addPathNodes(d),
                stroke = SolidColor(Color.Black),
                strokeLineWidth = 2f,
                strokeLineCap = StrokeCap.Round,
                strokeLineJoin = StrokeJoin.Round,
            )
        }
    }.build()

private fun fillIcon(name: String, vararg paths: String): ImageVector =
    ImageVector.Builder(name, 24.dp, 24.dp, 24f, 24f).apply {
        paths.forEach { d -> addPath(pathData = addPathNodes(d), fill = SolidColor(Color.Black)) }
    }.build()

// circle(cx, cy, r) as two arcs, since path data has no circle primitive.
private fun c(cx: Float, cy: Float, r: Float) =
    "M${cx - r} ${cy}a$r $r 0 1 0 ${2 * r} 0a$r $r 0 1 0 ${-2 * r} 0"

object StreamIcons {
    val Search = strokeIcon("search", c(11f, 11f, 7f), "M20 20l-4-4")
    val Home = strokeIcon("home", "M4 11l8-7 8 7v9h-5v-6H9v6H4z")
    val Films = strokeIcon(
        "films",
        "M5 4h14a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z",
        "M7 4v16M17 4v16M3 9h4M3 15h4M17 9h4M17 15h4",
    )
    val Series = strokeIcon(
        "series",
        "M5 5h14a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2z",
        "M8 21h8M12 17v4",
    )
    val Playlists = strokeIcon("playlists", "M4 6h11M4 11h11M4 16h7", c(17.5f, 17f, 2.5f), "M20 17V8")
    val Music = strokeIcon("music", c(7f, 18f, 3f), c(18f, 16f, 3f), "M10 18V5l11-2v13")
    val History = strokeIcon("history", "M3 12a9 9 0 1 0 3-6.7L3 8", "M3 3v5h5", "M12 7v5l3 2")
    val Library = strokeIcon("library", "M4 5h4v14H4zM10 5h4v14h-4z", "M16 6l3.5-1 3 13.5-3.5 1z")
    val Restart = strokeIcon("restart", "M4 12a8 8 0 1 0 2.4-5.7", "M4 4v4h4")
    val Forward = strokeIcon("forward", "M20 12a8 8 0 1 1-2.4-5.7", "M20 4v4h-4")
    val Add = strokeIcon("add", "M12 5v14M5 12h14")
    val Back = strokeIcon("back", "M19 12H5M12 19l-7-7 7-7")
    val Close = strokeIcon("close", "M6 6l12 12M18 6L6 18")
    val Info = strokeIcon("info", c(12f, 12f, 9f), "M12 11v6M12 7.5v.5")
    val Subtitles = strokeIcon(
        "subtitles",
        "M5 5h14a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2z",
        "M7 12h4M13 12h4M7 15.5h10",
    )
    val Audio = strokeIcon("audio", "M4 10v4h4l5 4V6L8 10z", "M16 9a4 4 0 0 1 0 6M18.5 6.5a8 8 0 0 1 0 11")
    val Settings = strokeIcon("settings", "M4 7h10M18 7h2M4 17h2M10 17h10", c(16f, 7f, 2f), c(8f, 17f, 2f))
    val ChevronDown = strokeIcon("chevronDown", "M6 9l6 6 6-6")
    val Episodes = strokeIcon("episodes", "M4 6h16M4 12h16M4 18h10")
    val Play = fillIcon("play", "M7 4.5v15l12.5-7.5z")
    val Pause = fillIcon("pause", "M7 5h3a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1zM14 5h3a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1h-3a1 1 0 0 1-1-1V6a1 1 0 0 1 1-1z")
    val SkipPrevious = fillIcon("skipPrevious", "M18 6v12l-9-6z", "M5 6h2v12H5z")
    val Minimize = strokeIcon(
        "minimize",
        "M5 4h14a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z",
        "M12 13h6v4h-6z",
    )
    val SkipNext = fillIcon("skipNext", "M6 6v12l9-6z", "M17 6h2v12h-2z")
}

@Composable
fun StreamIcon(icon: ImageVector, tint: Color, size: Dp = 16.dp, modifier: Modifier = Modifier) {
    Image(
        painter = rememberVectorPainter(icon),
        contentDescription = null,
        colorFilter = ColorFilter.tint(tint),
        modifier = modifier.size(size),
    )
}
