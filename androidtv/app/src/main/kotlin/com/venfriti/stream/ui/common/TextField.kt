package com.venfriti.stream.ui.common

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TonalColor

// Material 3 outlined field, pill-shaped: focus is the outline turning light.
// OK on it opens the TV's on-screen keyboard.
@Composable
fun StreamTextField(
    value: String,
    onValueChange: (String) -> Unit,
    placeholder: String,
    modifier: Modifier = Modifier,
    mono: Boolean = true,
    keyboardType: KeyboardType = KeyboardType.Uri,
    onDone: () -> Unit = {},
) {
    var focused by remember { mutableStateOf(false) }
    val shape = RoundedCornerShape(50)
    Box(
        modifier = modifier
            .fillMaxWidth()
            .height(48.dp)
            .background(if (focused) TextColor.copy(alpha = 0.16f) else TonalColor, shape)
            .border(2.dp, if (focused) TextColor else BorderColor, shape)
            .padding(horizontal = 20.dp),
        contentAlignment = Alignment.CenterStart,
    ) {
        val style = TextStyle(
            color = TextColor,
            fontSize = 16.sp,
            fontFamily = if (mono) MonoFontFamily else null,
        )
        if (value.isEmpty()) Text(placeholder, color = MutedColor, fontSize = 16.sp, fontFamily = if (mono) MonoFontFamily else null)
        BasicTextField(
            value = value,
            onValueChange = onValueChange,
            singleLine = true,
            textStyle = style,
            cursorBrush = SolidColor(GoldTextColor),
            keyboardOptions = KeyboardOptions(keyboardType = keyboardType, imeAction = ImeAction.Done),
            keyboardActions = KeyboardActions(onDone = { onDone() }),
            modifier = Modifier.fillMaxWidth().onFocusChanged { focused = it.isFocused },
        )
    }
}
