using System.Text.Json.Serialization;

namespace StreamVideosPlayer.Services;

// Mirrors playbackProgress in main.go.
public class PlaybackProgress
{
    [JsonPropertyName("position")]
    public double Position { get; set; }

    [JsonPropertyName("duration")]
    public double Duration { get; set; }

    [JsonPropertyName("updated")]
    public long Updated { get; set; }
}

// Mirrors the POST /api/progress request body handleProgress decodes.
public class ProgressUpdate
{
    [JsonPropertyName("key")]
    public string Key { get; set; } = "";

    [JsonPropertyName("position")]
    public double Position { get; set; }

    [JsonPropertyName("duration")]
    public double Duration { get; set; }
}
