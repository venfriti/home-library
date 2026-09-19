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
