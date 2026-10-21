package com.venfriti.stream.ui.common

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.focus.focusProperties
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Shape
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Border
import androidx.tv.material3.Card
import androidx.tv.material3.CardDefaults
import androidx.tv.material3.Glow
import androidx.tv.material3.Text
import coil.compose.AsyncImage
import coil.request.ImageRequest
import com.venfriti.stream.ui.theme.GoldColor
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.InkColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.SansFontFamily
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TonalColor

// The one focusable building block every tile, row and button in the app is
// made of: tv-material3's Card (D-pad focus + click), with the focus look
// from the design canvas -- a lift (scale), a light outline and a soft dark
// glow underneath. Focus is the only "highlighted" signal a remote gives, so
// it has to read from across the room.
@Composable
fun FocusSurface(
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    shape: Shape = RoundedCornerShape(6.dp),
    containerColor: Color = Color.Transparent,
    focusedContainerColor: Color = containerColor,
    focusedScale: Float = 1.06f,
    outline: Boolean = true,
    glow: Boolean = true,
    onLongClick: (() -> Unit)? = null,
    onFocus: (Boolean) -> Unit = {},
    contentModifier: Modifier = Modifier,
    content: @Composable BoxScope.(focused: Boolean) -> Unit,
) {
    var focused by remember { mutableStateOf(false) }
    Card(
        onClick = onClick,
        onLongClick = onLongClick,
        modifier = modifier.onFocusChanged {
            if (focused != it.isFocused) {
                focused = it.isFocused
                onFocus(it.isFocused)
            }
        },
        shape = CardDefaults.shape(shape, shape, shape),
        colors = CardDefaults.colors(
            containerColor = containerColor,
            focusedContainerColor = focusedContainerColor,
            pressedContainerColor = focusedContainerColor,
        ),
        scale = CardDefaults.scale(focusedScale = focusedScale, pressedScale = focusedScale),
        border = CardDefaults.border(
            focusedBorder = if (outline) Border(border = BorderStroke(2.dp, TextColor), shape = shape) else Border.None,
        ),
        glow = CardDefaults.glow(
            focusedGlow = if (glow) Glow(elevationColor = Color.Black.copy(alpha = 0.7f), elevation = 14.dp) else Glow.None,
        ),
    ) {
        Box(modifier = contentModifier) { content(focused) }
    }
}

enum class ButtonKind { Primary, Tonal, PlayerPrimary }

// Buttons: Primary is the gold "RESUME"/"PLAY" from the web hero; Tonal is a
// Material 3 tonal button that flips to the inverse (light) fill on focus.
// `mono` picks the web's uppercase mono label style (the wall); off gives the
// Material sentence-case pill (the player and dialogs).
@Composable
fun StreamButton(
    label: String,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    icon: ImageVector? = null,
    kind: ButtonKind = ButtonKind.Tonal,
    mono: Boolean = true,
    height: Dp = 38.dp,
    fillWidth: Boolean = false,
    leading: (@Composable (Color) -> Unit)? = null,
    trailingIcon: ImageVector? = null,
) {
    val shape = if (mono) RoundedCornerShape(4.dp) else RoundedCornerShape(50)
    val base = when (kind) {
        ButtonKind.Primary -> GoldColor
        ButtonKind.PlayerPrimary -> GoldTextColor
        ButtonKind.Tonal -> TonalColor
    }
    val focusedBase = if (kind == ButtonKind.Tonal) TextColor else base
    FocusSurface(
        onClick = onClick,
        modifier = modifier.height(height).let { if (fillWidth) it.fillMaxWidth() else it },
        shape = shape,
        containerColor = base,
        focusedContainerColor = focusedBase,
        focusedScale = 1.05f,
        outline = kind != ButtonKind.Tonal,
        contentModifier = Modifier.fillMaxHeight().let { if (fillWidth) it.fillMaxWidth() else it },
    ) { focused ->
        val fg = if (kind != ButtonKind.Tonal || focused) InkColor else TextColor
        Row(
            modifier = Modifier.align(Alignment.Center).padding(horizontal = if (mono) 19.dp else 22.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            leading?.invoke(fg)
            if (icon != null) StreamIcon(icon, tint = fg, size = if (mono) 13.dp else 15.dp)
            Text(
                text = if (mono) label.uppercase() else label,
                color = fg,
                fontFamily = if (mono) MonoFontFamily else SansFontFamily,
                fontWeight = if (mono) FontWeight.Normal else FontWeight.SemiBold,
                fontSize = if (mono) 12.sp else 15.sp,
                letterSpacing = if (mono) 1.5.sp else 0.sp,
            )
            if (trailingIcon != null) StreamIcon(trailingIcon, tint = fg, size = if (mono) 13.dp else 16.dp)
        }
    }
}

// Round icon-only button: Material 3 tonal icon button, or the filled gold
// play/pause at the centre of the transport row. `quiet` drops the resting
// fill (a bare glyph) and keeps only the focused fill, since on a remote the
// fill is the one sign of where focus is.
@Composable
fun IconCircleButton(
    icon: ImageVector,
    description: String,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    size: Dp = 36.dp,
    iconSize: Dp = 16.dp,
    primary: Boolean = false,
    quiet: Boolean = false,
    // Disabled: dimmed and skipped by D-pad focus, but keeps its slot so the
    // row doesn't reflow (e.g. Previous on a season's first episode).
    enabled: Boolean = true,
    badge: String? = null,
) {
    FocusSurface(
        onClick = { if (enabled) onClick() },
        modifier = modifier
            .size(size)
            .focusProperties { canFocus = enabled }
            .alpha(if (enabled) 1f else 0.35f)
            .semantics { contentDescription = description },
        shape = CircleShape,
        containerColor = when {
            primary -> GoldTextColor
            quiet -> Color.Transparent
            else -> TonalColor
        },
        focusedContainerColor = if (primary) GoldTextColor else TextColor,
        focusedScale = 1.1f,
        outline = primary,
        contentModifier = Modifier.size(size),
    ) { focused ->
        val fg = if (primary || focused) InkColor else TextColor
        StreamIcon(icon, tint = fg, size = iconSize, modifier = Modifier.align(Alignment.Center))
        if (badge != null) {
            Text(
                text = badge,
                color = fg,
                fontFamily = SansFontFamily,
                fontWeight = FontWeight.SemiBold,
                fontSize = (iconSize.value * 0.36f).sp,
                modifier = Modifier.align(Alignment.Center).padding(top = 1.dp),
            )
        }
    }
}

// A 3dp gold sliver pinned to the bottom of the art (.pc-prog/.cw-bar).
@Composable
fun ProgressBar(progress: Double, modifier: Modifier = Modifier, color: Color = GoldColor) {
    if (progress <= 0) return
    Box(
        modifier = modifier
            .fillMaxWidth()
            .height(3.dp)
            .background(Color.White.copy(alpha = 0.22f)),
    ) {
        Box(
            modifier = Modifier
                .fillMaxHeight()
                .fillMaxWidth(fraction = progress.coerceIn(0.02, 1.0).toFloat())
                .background(color),
        )
    }
}

@Composable
fun Artwork(
    url: String?,
    description: String?,
    modifier: Modifier = Modifier,
    alignment: Alignment = Alignment.Center,
    model: Any? = null,
) {
    val context = LocalContext.current
    val data = model ?: url ?: return
    AsyncImage(
        model = remember(data) { ImageRequest.Builder(context).data(data).crossfade(250).build() },
        contentDescription = description,
        contentScale = ContentScale.Crop,
        alignment = alignment,
        modifier = modifier,
    )
}
