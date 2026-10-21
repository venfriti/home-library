package com.venfriti.stream.network

import java.io.IOException
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.InetAddress
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import okhttp3.HttpUrl.Companion.toHttpUrlOrNull
import okhttp3.OkHttpClient
import okhttp3.Request

sealed interface ProbeResult {
    data object Ok : ProbeResult
    // Nothing answered: refused, timed out, no route. The server is off,
    // asleep or still starting -- or the address is wrong.
    data class Unreachable(val detail: String) : ProbeResult
    // Something answered, but not the way Stream Videos does.
    data class NotStream(val detail: String) : ProbeResult
    // It's the server, but it wants a session: this TV isn't being treated
    // as on the home network (the LAN bypass didn't apply).
    data object Unauthorized : ProbeResult
}

// Short timeouts on purpose: the server is on the LAN, so if it hasn't
// answered in 3 seconds it isn't going to, and the retry screen should appear
// right away instead of after OkHttp's default 10s.
private val probeHttp = OkHttpClient.Builder()
    .connectTimeout(3, TimeUnit.SECONDS)
    .readTimeout(4, TimeUnit.SECONDS)
    .callTimeout(6, TimeUnit.SECONDS)
    .followRedirects(false)
    .build()

// A cheap "are you there, and are you Stream?" check that never touches
// /login (whose per-IP rate limit a 30-second heartbeat would trip).
//   1. GET /api/ping      -- the small route suggested for main.go; 200 = Ok.
//   2. otherwise GET /api/progress?key=__ping -- exists on every current
//      server: 200 on the LAN, 401 off it, 404/other = not this server.
suspend fun probeServer(baseUrl: String): ProbeResult = withContext(Dispatchers.IO) {
    val base = baseUrl.trimEnd('/') + "/"
    if (base.toHttpUrlOrNull() == null) return@withContext ProbeResult.NotStream("That isn't a valid address")
    try {
        probeHttp.newCall(Request.Builder().url(base + "api/ping").build()).execute().use { r ->
            if (r.code == 200) return@withContext ProbeResult.Ok
        }
        probeHttp.newCall(Request.Builder().url(base + "api/progress?key=__ping").build()).execute().use { r ->
            when (r.code) {
                200 -> ProbeResult.Ok
                401 -> ProbeResult.Unauthorized
                else -> ProbeResult.NotStream("It answered with HTTP ${r.code}")
            }
        }
    } catch (e: IOException) {
        ProbeResult.Unreachable(e.javaClass.simpleName)
    } catch (e: IllegalArgumentException) {
        ProbeResult.NotStream("That isn't a valid address")
    }
}

// Wake-on-LAN magic packet: 6 x 0xFF then the MAC 16 times, broadcast on
// UDP 9. Only helps if the server's BIOS/NIC has WoL turned on and it's on
// the same network segment as the TV.
suspend fun sendWakeOnLan(mac: String): Boolean = withContext(Dispatchers.IO) {
    val bytes = mac.split(':', '-', '.').filter { it.isNotEmpty() }.mapNotNull { it.toIntOrNull(16)?.toByte() }
    if (bytes.size != 6) return@withContext false
    val packet = ByteArray(6 + 16 * 6)
    for (i in 0 until 6) packet[i] = 0xFF.toByte()
    for (i in 0 until 16) for (j in 0 until 6) packet[6 + i * 6 + j] = bytes[j]
    try {
        DatagramSocket().use { s ->
            s.broadcast = true
            val target = InetAddress.getByName("255.255.255.255")
            s.send(DatagramPacket(packet, packet.size, target, 9))
            s.send(DatagramPacket(packet, packet.size, target, 7))
        }
        true
    } catch (e: IOException) {
        false
    }
}
