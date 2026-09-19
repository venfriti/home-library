using System.Text.Json.Serialization;

namespace StreamVideosPlayer.Services;

// Mirrors the playlist struct in main.go (see GET/POST /playlists).
public class PlaylistDto
{
    [JsonPropertyName("id")]
    public string Id { get; set; } = "";

    [JsonPropertyName("name")]
    public string Name { get; set; } = "";

    [JsonPropertyName("items")]
    public List<string> Items { get; set; } = new();

    [JsonPropertyName("created")]
    public long Created { get; set; }

    [JsonPropertyName("updated")]
    public long Updated { get; set; }

    // Populated by MainWindow.LoadPlaylistsAsync via
    // ServerClient.GetPlaylistCoverAsync, not part of the /playlists JSON
    // itself -- null for an empty playlist (nothing to compose a cover
    // from) or a failed fetch, matching .pl-empty's "no cover" state on the
    // web either way.
    public System.Windows.Media.Imaging.BitmapImage? CoverImage { get; set; }
}

// Body shape for POST /playlists -- action selects which fields matter (see
// handlePlaylists in main.go: create/add/remove/rename/reorder/delete).
public class PlaylistAction
{
    [JsonPropertyName("action")]
    public string Action { get; set; } = "";

    [JsonPropertyName("id")]
    public string? Id { get; set; }

    [JsonPropertyName("name")]
    public string? Name { get; set; }

    [JsonPropertyName("key")]
    public string? Key { get; set; }
}
