using System.Net;
using System.Net.Http;
using System.Text;
using System.Text.Json;

namespace StreamVideosPlayer.Services;

// Talks to the existing Go server (main.go) over plain HTTP(S) -- no server
// changes required. Two HttpClients share one CookieContainer so the session
// cookie set by /login is attached to both:
//   - _http follows redirects, for ordinary JSON API calls.
//   - _httpNoRedirect stops at the first redirect, needed to read /login's
//     302-vs-401 outcome and to read the Location header /file?key= hands
//     back (see ResolvePlaybackUrlAsync).
public class ServerClient
{
    private readonly CookieContainer _cookies = new();
    private readonly HttpClient _http;
    private readonly HttpClient _httpNoRedirect;
    private string? _password;

    public ServerClient(string baseUrl)
    {
        var normalized = baseUrl.TrimEnd('/') + "/";
        var baseAddress = new Uri(normalized);

        _http = new HttpClient(new HttpClientHandler
        {
            CookieContainer = _cookies,
            AllowAutoRedirect = true,
        })
        { BaseAddress = baseAddress };

        _httpNoRedirect = new HttpClient(new HttpClientHandler
        {
            CookieContainer = _cookies,
            AllowAutoRedirect = false,
        })
        { BaseAddress = baseAddress };
    }

    // Mirrors handleLogin in main.go: POST password, 302 (with the session
    // cookie attached) on success, 401 on a wrong password, 429 if the
    // server's per-IP rate limit has tripped.
    public async Task<bool> LoginAsync(string password)
    {
        using var content = new FormUrlEncodedContent(new Dictionary<string, string>
        {
            ["password"] = password,
        });
        using var resp = await _httpNoRedirect.PostAsync("login", content);
        if (resp.StatusCode != HttpStatusCode.Found)
            return false;

        _password = password;
        return true;
    }

    public async Task<List<FileEntry>> GetFilesAsync()
    {
        using var resp = await SendAuthedAsync(() => new HttpRequestMessage(HttpMethod.Get, "files"));
        resp.EnsureSuccessStatusCode();
        var json = await resp.Content.ReadAsStringAsync();
        return JsonSerializer.Deserialize<List<FileEntry>>(json) ?? new List<FileEntry>();
    }

    // GET /local/files is localOnly-gated in main.go (only answers requests
    // whose TCP connection is loopback/LAN), so this only ever succeeds when
    // this app and the server are on the same network -- exactly the
    // situation the Local/Online toggle is for. Local entries carry no
    // thumb_url from the API at all (see handleLocalFiles); this fills one
    // in pointing at /local/thumbnail, which is safe to load with no cookie
    // or token because isAuthed (and localOnly above it) both just re-check
    // the same LAN origin the image request itself is already coming from.
    public async Task<List<FileEntry>> GetLocalFilesAsync()
    {
        using var resp = await SendAuthedAsync(() => new HttpRequestMessage(HttpMethod.Get, "local/files"));
        resp.EnsureSuccessStatusCode();
        var json = await resp.Content.ReadAsStringAsync();
        var entries = JsonSerializer.Deserialize<List<FileEntry>>(json) ?? new List<FileEntry>();
        foreach (var e in entries)
        {
            if (!e.IsVideo) continue;
            if (string.IsNullOrEmpty(e.ThumbUrl) && string.IsNullOrEmpty(e.PosterUrl))
                e.ThumbUrl = _http.BaseAddress + "local/thumbnail?path=" + Uri.EscapeDataString(e.Key);
            // handleLocalFiles never reports whether a subtitle actually
            // exists (unlike the CDN catalog's subtitle_url) -- attach it
            // unconditionally, same as the web UI's own local-file branch;
            // libVLC just skips a subtitle track it can't fetch rather than
            // failing playback.
            e.SubtitleUrl = _http.BaseAddress + "local/subtitle?path=" + Uri.EscapeDataString(e.Key);
        }
        return entries;
    }

    public async Task<List<PlaylistDto>> GetPlaylistsAsync()
    {
        using var resp = await SendAuthedAsync(() => new HttpRequestMessage(HttpMethod.Get, "playlists"));
        resp.EnsureSuccessStatusCode();
        var json = await resp.Content.ReadAsStringAsync();
        return JsonSerializer.Deserialize<List<PlaylistDto>>(json) ?? new List<PlaylistDto>();
    }

    // Returns null when no release has been published (see
    // handleWindowsAppVersion in main.go) or the check itself fails --
    // either way, an update check is never worth interrupting the user over.
    public async Task<UpdateInfo?> GetLatestVersionAsync()
    {
        try
        {
            using var resp = await SendAuthedAsync(() => new HttpRequestMessage(HttpMethod.Get, "windows-app/version"));
            if (!resp.IsSuccessStatusCode) return null;
            var json = await resp.Content.ReadAsStringAsync();
            var info = JsonSerializer.Deserialize<UpdateInfo>(json);
            return string.IsNullOrEmpty(info?.Version) ? null : info;
        }
        catch
        {
            return null;
        }
    }

    public Task<PlaylistDto?> CreatePlaylistAsync(string name, string? seedKey = null) =>
        PostPlaylistActionAsync(new PlaylistAction { Action = "create", Name = name, Key = seedKey });

    public Task<PlaylistDto?> AddToPlaylistAsync(string playlistId, string key) =>
        PostPlaylistActionAsync(new PlaylistAction { Action = "add", Id = playlistId, Key = key });

    public Task<PlaylistDto?> RemoveFromPlaylistAsync(string playlistId, string key) =>
        PostPlaylistActionAsync(new PlaylistAction { Action = "remove", Id = playlistId, Key = key });

    public async Task DeletePlaylistAsync(string playlistId) =>
        await PostPlaylistActionAsync(new PlaylistAction { Action = "delete", Id = playlistId });

    // Mirrors handlePlaylists' POST contract in main.go: the response body is
    // the updated playlist, or a bare "{}" when the action doesn't produce
    // one (delete, or an add/remove/rename against an id that doesn't
    // exist) -- deserializing "{}" against PlaylistDto just yields an empty
    // (Id == "") instance rather than throwing, so callers check that.
    private async Task<PlaylistDto?> PostPlaylistActionAsync(PlaylistAction action)
    {
        var json = JsonSerializer.Serialize(action);
        using var resp = await SendAuthedAsync(() => new HttpRequestMessage(HttpMethod.Post, "playlists")
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json"),
        });
        resp.EnsureSuccessStatusCode();
        var body = await resp.Content.ReadAsStringAsync();
        var result = JsonSerializer.Deserialize<PlaylistDto>(body);
        return string.IsNullOrEmpty(result?.Id) ? null : result;
    }

    // Resolves a FileEntry to a URL LibVLC can open directly.
    //
    // An HLS entry already carries a ready-to-use public CDN base (see
    // fileEntry.HLSBase in main.go's handleFiles). A direct-play entry only
    // has an authenticated redirector (/file?key=...), which 302s straight to
    // the public r2_public_url once the caller is authenticated (see
    // handleFile) -- so we follow that redirect ourselves here and hand
    // LibVLC the real, unauthenticated CDN URL it points to. That means
    // LibVLC itself never needs to hold a cookie or token at all.
    public async Task<string> ResolvePlaybackUrlAsync(FileEntry entry)
    {
        // Same LAN-origin reasoning as GetLocalFilesAsync's thumbnail URLs:
        // /local/file needs no cookie or token here, only a request that
        // actually originates on the LAN, which this one does.
        if (entry.IsLocal)
            return _http.BaseAddress + "local/file?path=" + Uri.EscapeDataString(entry.Key);

        if (!string.IsNullOrEmpty(entry.HlsBase))
            return entry.HlsBase.TrimEnd('/') + "/master.m3u8";

        var location = await FollowFileRedirectAsync(entry.Key);
        if (location is null && _password is not null && await LoginAsync(_password))
            location = await FollowFileRedirectAsync(entry.Key);

        return location ?? throw new InvalidOperationException(
            $"server did not return a playable URL for '{entry.Key}'");
    }

    private async Task<string?> FollowFileRedirectAsync(string key)
    {
        using var resp = await _httpNoRedirect.GetAsync("file?key=" + Uri.EscapeDataString(key));
        return resp.StatusCode == HttpStatusCode.Found ? resp.Headers.Location?.ToString() : null;
    }

    public async Task<PlaybackProgress?> GetProgressAsync(string key)
    {
        using var resp = await SendAuthedAsync(
            () => new HttpRequestMessage(HttpMethod.Get, "api/progress?key=" + Uri.EscapeDataString(key)));
        if (!resp.IsSuccessStatusCode) return null;

        var json = await resp.Content.ReadAsStringAsync();
        if (string.IsNullOrWhiteSpace(json) || json == "{}") return null;

        var progress = JsonSerializer.Deserialize<PlaybackProgress>(json);
        return progress is { Duration: > 0 } ? progress : null;
    }

    // One request for every saved position, for the library grid's progress
    // slivers -- see the "all" branch of handleProgress in main.go, which
    // exists for exactly this (its own comment: "the library view needs
    // every saved position at once ... rather than one request per tile").
    public async Task<Dictionary<string, PlaybackProgress>> GetAllProgressAsync()
    {
        try
        {
            using var resp = await SendAuthedAsync(() => new HttpRequestMessage(HttpMethod.Get, "api/progress?all=1"));
            if (!resp.IsSuccessStatusCode) return new Dictionary<string, PlaybackProgress>();
            var json = await resp.Content.ReadAsStringAsync();
            return JsonSerializer.Deserialize<Dictionary<string, PlaybackProgress>>(json)
                   ?? new Dictionary<string, PlaybackProgress>();
        }
        catch
        {
            return new Dictionary<string, PlaybackProgress>();
        }
    }

    public async Task PostProgressAsync(string key, double positionSeconds, double durationSeconds)
    {
        var body = new ProgressUpdate { Key = key, Position = positionSeconds, Duration = durationSeconds };
        var json = JsonSerializer.Serialize(body);

        // Best-effort: a dropped update just means resume falls back to
        // wherever the last successful save left off.
        using var resp = await SendAuthedAsync(() => new HttpRequestMessage(HttpMethod.Post, "api/progress")
        {
            Content = new StringContent(json, Encoding.UTF8, "application/json"),
        });
    }

    // Runs a request built fresh by requestFactory, and if the session cookie
    // has expired (7 days -- see sessionTTL in main.go), silently re-logs in
    // with the password from the last successful LoginAsync and retries once.
    private async Task<HttpResponseMessage> SendAuthedAsync(Func<HttpRequestMessage> requestFactory)
    {
        var resp = await _http.SendAsync(requestFactory());
        if (resp.StatusCode == HttpStatusCode.Unauthorized && _password is not null)
        {
            resp.Dispose();
            if (await LoginAsync(_password))
                resp = await _http.SendAsync(requestFactory());
        }
        return resp;
    }
}
