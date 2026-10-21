package com.venfriti.stream.network

import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.PlaybackProgress
import com.venfriti.stream.data.PlaylistAction
import com.venfriti.stream.data.PlaylistDto
import com.venfriti.stream.data.ProgressUpdate
import com.venfriti.stream.data.TrickplayInfo
import com.venfriti.stream.data.UpdateInfo
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import okhttp3.Cookie
import okhttp3.CookieJar
import okhttp3.FormBody
import okhttp3.HttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response

private val json = Json { ignoreUnknownKeys = true }

// A plain in-memory CookieJar, keyed by host -- OkHttp has no built-in
// implementation beyond a no-op default (unlike .NET's CookieContainer,
// which HttpClient already carries one of). Session-only, same as the
// Windows client's own CookieContainer: nothing here needs to survive an
// app restart, since a fresh login() re-establishes the session anyway.
private class InMemoryCookieJar : CookieJar {
    private val store = mutableMapOf<String, List<Cookie>>()

    override fun saveFromResponse(url: HttpUrl, cookies: List<Cookie>) {
        store[url.host] = cookies
    }

    override fun loadForRequest(url: HttpUrl): List<Cookie> = store[url.host] ?: emptyList()
}

// Talks to the existing Go server (main.go) over plain HTTP(S) -- no server
// changes required. Mirrors ServerClient.cs on the Windows client field for
// field: two OkHttpClients share one CookieJar so the session cookie
// /login sets is attached to both -- one follows redirects for ordinary
// JSON calls, the other doesn't, needed to read /login's 302-vs-401 outcome
// and /file?key='s Location header (see resolvePlaybackUrl).
class ServerClient(baseUrl: String) {
    private val baseUrl = baseUrl.trimEnd('/') + "/"
    val address: String = baseUrl.trimEnd('/')
    private var password: String? = null

    private val cookieJar = InMemoryCookieJar()

    // 3s connect: the server is on the LAN, so a dead server should fail
    // fast and hand over to the reconnect logic instead of hanging on
    // OkHttp's 10s default. Reads stay generous for the /files listing.
    private val http = OkHttpClient.Builder()
        .connectTimeout(3, java.util.concurrent.TimeUnit.SECONDS)
        .readTimeout(20, java.util.concurrent.TimeUnit.SECONDS)
        .cookieJar(cookieJar)
        .followRedirects(true)
        .build()

    private val httpNoRedirect = http.newBuilder()
        .followRedirects(false)
        .build()

    // Mirrors handleLogin in main.go: POST password, 302 (with the session
    // cookie attached) on success, 401 on a wrong password, 429 if the
    // server's per-IP rate limit has tripped.
    suspend fun login(password: String): Boolean = withContext(Dispatchers.IO) {
        val body = FormBody.Builder().add("password", password).build()
        val request = Request.Builder().url(baseUrl + "login").post(body).build()
        httpNoRedirect.newCall(request).execute().use { resp ->
            if (resp.code != 302) return@withContext false
            this@ServerClient.password = password
            true
        }
    }

    suspend fun getFiles(): List<FileEntry> = withContext(Dispatchers.IO) {
        sendAuthed { Request.Builder().url(baseUrl + "files").build() }.use { resp ->
            json.decodeFromString(resp.body!!.string())
        }
    }

    // GET /local/files is localOnly-gated in main.go (only answers requests
    // whose TCP connection is loopback/LAN), so this only ever succeeds
    // when the device and the server are on the same network -- exactly
    // the situation the Local/Online toggle is for. Local entries carry no
    // thumb_url from the API at all; this fills one in pointing at
    // /local/thumbnail, safe to load with no cookie or token because
    // isAuthed (and localOnly above it) both just re-check the same LAN
    // origin the image request itself is already coming from.
    suspend fun getLocalFiles(): List<FileEntry> = withContext(Dispatchers.IO) {
        val entries: List<FileEntry> = sendAuthed { Request.Builder().url(baseUrl + "local/files").build() }
            .use { resp -> json.decodeFromString(resp.body!!.string()) }
        entries.forEach { e ->
            if (!e.isVideo) return@forEach
            // Always a real frame grab, even when a poster also exists:
            // cardImageUrl (Films/TV rows) still prefers posterUrl, but
            // thumbnailImageUrl (Continue watching/Music videos rows) is
            // meant to show the video itself, not its poster.
            if (e.thumbUrl.isNullOrEmpty()) {
                e.thumbUrl = baseUrl + "local/thumbnail?path=" + java.net.URLEncoder.encode(e.key, "UTF-8")
            }
            e.subtitleUrl = baseUrl + "local/subtitle?path=" + java.net.URLEncoder.encode(e.key, "UTF-8")
        }
        entries
    }

    suspend fun getPlaylists(): List<PlaylistDto> = withContext(Dispatchers.IO) {
        sendAuthed { Request.Builder().url(baseUrl + "playlists").build() }.use { resp ->
            json.decodeFromString(resp.body!!.string())
        }
    }

    // /playlists/cover requires a real session (see handlePlaylistCover in
    // main.go) with no LAN-bypass equivalent to /local/thumbnail's, so
    // unlike every other image URL here it can't just be handed straight to
    // Coil as a plain URL -- nothing would attach the session cookie.
    // Fetching the bytes through this client (which already carries it) and
    // handing Coil the raw bytes is the workaround (see PlaylistRow).
    suspend fun getPlaylistCoverBytes(id: String, updated: Long): ByteArray? = withContext(Dispatchers.IO) {
        try {
            sendAuthed {
                Request.Builder().url(baseUrl + "playlists/cover?id=" + java.net.URLEncoder.encode(id, "UTF-8") + "&v=$updated").build()
            }.use { resp -> if (resp.isSuccessful) resp.body!!.bytes() else null }
        } catch (e: Exception) {
            null
        }
    }

    // Returns null when no release has been published, or the check itself
    // fails -- either way, an update check is never worth interrupting the
    // user over.
    suspend fun getLatestVersion(): UpdateInfo? = withContext(Dispatchers.IO) {
        try {
            sendAuthed { Request.Builder().url(baseUrl + "androidtv-app/version").build() }.use { resp ->
                if (!resp.isSuccessful) return@withContext null
                val info: UpdateInfo = json.decodeFromString(resp.body!!.string())
                info.takeIf { it.version.isNotEmpty() }
            }
        } catch (e: Exception) {
            null
        }
    }

    suspend fun createPlaylist(name: String, seedKey: String? = null) =
        postPlaylistAction(PlaylistAction(action = "create", name = name, key = seedKey))

    suspend fun addToPlaylist(playlistId: String, key: String) =
        postPlaylistAction(PlaylistAction(action = "add", id = playlistId, key = key))

    suspend fun removeFromPlaylist(playlistId: String, key: String) =
        postPlaylistAction(PlaylistAction(action = "remove", id = playlistId, key = key))

    suspend fun deletePlaylist(playlistId: String) {
        postPlaylistAction(PlaylistAction(action = "delete", id = playlistId))
    }

    // Mirrors handlePlaylists' POST contract in main.go: the response body
    // is the updated playlist, or a bare "{}" when the action doesn't
    // produce one (delete, or an add/remove/rename against an id that
    // doesn't exist) -- decoding "{}" against PlaylistDto just yields an
    // empty (id == "") instance rather than throwing, so callers check that.
    private suspend fun postPlaylistAction(action: PlaylistAction): PlaylistDto? = withContext(Dispatchers.IO) {
        val body = json.encodeToString(action).toRequestBody("application/json".toMediaType())
        sendAuthed { Request.Builder().url(baseUrl + "playlists").post(body).build() }.use { resp ->
            val result: PlaylistDto = json.decodeFromString(resp.body!!.string())
            result.takeIf { it.id.isNotEmpty() }
        }
    }

    // Resolves a FileEntry to a URL ExoPlayer can open directly.
    //
    // An HLS entry already carries a ready-to-use public CDN base. A
    // direct-play entry only has an authenticated redirector
    // (/file?key=...), which 302s straight to the public r2_public_url once
    // the caller is authenticated -- so this follows that redirect itself
    // and hands ExoPlayer the real, unauthenticated CDN URL it points to.
    // ExoPlayer itself never needs to hold a cookie or token at all.
    suspend fun resolvePlaybackUrl(entry: FileEntry): String = withContext(Dispatchers.IO) {
        if (entry.isLocal) {
            return@withContext baseUrl + "local/file?path=" + java.net.URLEncoder.encode(entry.key, "UTF-8")
        }
        if (!entry.hlsBase.isNullOrEmpty()) {
            return@withContext entry.hlsBase.trimEnd('/') + "/master.m3u8"
        }

        var location = followFileRedirect(entry.key)
        if (location == null && password != null && login(password!!)) {
            location = followFileRedirect(entry.key)
        }
        location ?: throw IllegalStateException("server did not return a playable URL for '${entry.key}'")
    }

    private suspend fun followFileRedirect(key: String): String? = withContext(Dispatchers.IO) {
        val request = Request.Builder().url(baseUrl + "file?key=" + java.net.URLEncoder.encode(key, "UTF-8")).build()
        httpNoRedirect.newCall(request).execute().use { resp ->
            if (resp.code == 302) resp.header("Location") else null
        }
    }

    // Scrub-preview sheets (local files only). The first ask for a video
    // starts generation server-side and returns ready=false; ask again later.
    suspend fun getTrickplay(key: String): TrickplayInfo? = withContext(Dispatchers.IO) {
        try {
            sendAuthed { Request.Builder().url(baseUrl + "local/trickplay?path=" + java.net.URLEncoder.encode(key, "UTF-8")).build() }
                .use { resp -> if (resp.isSuccessful) json.decodeFromString<TrickplayInfo>(resp.body!!.string()) else null }
        } catch (e: Exception) {
            null
        }
    }

    fun trickplaySheetUrl(key: String, sheet: Int): String =
        baseUrl + "local/trickplay?path=" + java.net.URLEncoder.encode(key, "UTF-8") + "&sheet=" + sheet

    suspend fun getProgress(key: String): PlaybackProgress? = withContext(Dispatchers.IO) {
        sendAuthed { Request.Builder().url(baseUrl + "api/progress?key=" + java.net.URLEncoder.encode(key, "UTF-8")).build() }
            .use { resp ->
                if (!resp.isSuccessful) return@withContext null
                val text = resp.body!!.string()
                if (text.isBlank() || text == "{}") return@withContext null
                val progress: PlaybackProgress = json.decodeFromString(text)
                progress.takeIf { it.duration > 0 }
            }
    }

    // One request for every saved position, for the wall's progress
    // slivers -- see the "all" branch of handleProgress in main.go.
    suspend fun getAllProgress(): Map<String, PlaybackProgress> = withContext(Dispatchers.IO) {
        try {
            sendAuthed { Request.Builder().url(baseUrl + "api/progress?all=1").build() }.use { resp ->
                if (!resp.isSuccessful) return@withContext emptyMap()
                json.decodeFromString(resp.body!!.string())
            }
        } catch (e: Exception) {
            emptyMap()
        }
    }

    suspend fun postProgress(key: String, positionSeconds: Double, durationSeconds: Double) = withContext(Dispatchers.IO) {
        val update = ProgressUpdate(key = key, position = positionSeconds, duration = durationSeconds)
        val body = json.encodeToString(update).toRequestBody("application/json".toMediaType())
        // Best-effort: a dropped update just means resume falls back to
        // wherever the last successful save left off.
        try {
            sendAuthed { Request.Builder().url(baseUrl + "api/progress").post(body).build() }.close()
        } catch (e: Exception) {
            // ignored
        }
    }

    // Runs a request built fresh by requestFactory, and if the session
    // cookie has expired (7 days -- see sessionTTL in main.go), silently
    // re-logs in with the password from the last successful login() and
    // retries once.
    private suspend fun sendAuthed(requestFactory: () -> Request): Response {
        var resp = http.newCall(requestFactory()).execute()
        if (resp.code == 401 && password != null) {
            resp.close()
            if (login(password!!)) {
                resp = http.newCall(requestFactory()).execute()
            }
        }
        return resp
    }
}
