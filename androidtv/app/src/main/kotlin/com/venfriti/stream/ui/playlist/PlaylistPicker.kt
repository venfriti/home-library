package com.venfriti.stream.ui.playlist

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.PlaylistDto
import com.venfriti.stream.ui.common.ButtonKind
import com.venfriti.stream.ui.common.FocusSurface
import com.venfriti.stream.ui.common.StreamButton
import com.venfriti.stream.ui.common.StreamIcon
import com.venfriti.stream.ui.common.StreamIcons
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.InkColor
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SansFontFamily
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor

// "Hold OK on a title": add it to (or take it out of) a playlist, or start a
// new one with it. Uses the POST /playlists actions ServerClient already had
// but nothing on TV called.
@Composable
fun PlaylistPicker(
    entry: FileEntry,
    playlists: List<PlaylistDto>,
    onToggle: (PlaylistDto, Boolean) -> Unit,
    onCreate: (String) -> Unit,
    onDismiss: () -> Unit,
) {
    var creating by remember { mutableStateOf(false) }
    var name by remember { mutableStateOf("") }
    val firstRequester = remember { FocusRequester() }
    val fieldRequester = remember { FocusRequester() }

    BackHandler { if (creating) creating = false else onDismiss() }
    LaunchedEffect(creating) {
        runCatching { if (creating) fieldRequester.requestFocus() else firstRequester.requestFocus() }
    }

    Box(modifier = Modifier.fillMaxSize().background(Color(0xB8040505)), contentAlignment = Alignment.Center) {
        Column(
            modifier = Modifier
                .width(420.dp)
                .background(Color(0xFF1A1D1D), RoundedCornerShape(28.dp))
                .padding(28.dp),
            verticalArrangement = Arrangement.spacedBy(6.dp),
        ) {
            Text("Add to playlist", color = Color.White, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 22.sp)
            Text(
                entry.continueWatchingLine,
                color = MutedColor,
                fontSize = 14.sp,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.padding(bottom = 12.dp),
            )

            if (creating) {
                var focused by remember { mutableStateOf(false) }
                Box(
                    modifier = Modifier
                        .fillMaxWidth()
                        .height(48.dp)
                        .border(2.dp, if (focused) TextColor else BorderColor, RoundedCornerShape(50))
                        .padding(horizontal = 20.dp),
                    contentAlignment = Alignment.CenterStart,
                ) {
                    if (name.isEmpty()) Text("Playlist name", color = MutedColor, fontSize = 16.sp)
                    BasicTextField(
                        value = name,
                        onValueChange = { name = it },
                        singleLine = true,
                        textStyle = TextStyle(color = TextColor, fontSize = 16.sp, fontFamily = SansFontFamily),
                        cursorBrush = SolidColor(GoldTextColor),
                        keyboardOptions = KeyboardOptions(imeAction = ImeAction.Done),
                        keyboardActions = KeyboardActions(onDone = {
                            if (name.isNotBlank()) { onCreate(name.trim()); onDismiss() }
                        }),
                        modifier = Modifier.fillMaxWidth().focusRequester(fieldRequester).onFocusChanged { focused = it.isFocused },
                    )
                }
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.padding(top = 10.dp)) {
                    StreamButton(
                        label = "Create",
                        kind = ButtonKind.PlayerPrimary,
                        mono = false,
                        height = 44.dp,
                        onClick = { if (name.isNotBlank()) { onCreate(name.trim()); onDismiss() } },
                    )
                    StreamButton(label = "Cancel", mono = false, height = 44.dp, onClick = { creating = false })
                }
            } else {
                playlists.forEachIndexed { i, p ->
                    val contains = entry.key in p.items
                    PickerRow(
                        label = p.name,
                        trailing = if (contains) "ADDED" else "${p.items.size}",
                        highlight = contains,
                        modifier = if (i == 0) Modifier.focusRequester(firstRequester) else Modifier,
                        onClick = { onToggle(p, contains) },
                    )
                }
                PickerRow(
                    label = "New playlist…",
                    trailing = null,
                    highlight = false,
                    icon = true,
                    modifier = if (playlists.isEmpty()) Modifier.focusRequester(firstRequester) else Modifier,
                    onClick = { creating = true },
                )
            }
        }
    }
}

@Composable
private fun PickerRow(
    label: String,
    trailing: String?,
    highlight: Boolean,
    modifier: Modifier = Modifier,
    icon: Boolean = false,
    onClick: () -> Unit,
) {
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
            if (icon) StreamIcon(StreamIcons.Add, tint = fg, size = 16.dp)
            Text(label, color = fg, fontSize = 15.sp, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.weight(1f))
            if (trailing != null) {
                Text(
                    trailing,
                    color = if (focused) InkColor.copy(alpha = 0.6f) else if (highlight) GoldTextColor else MutedColor,
                    fontSize = 11.sp,
                )
            }
        }
    }
}
