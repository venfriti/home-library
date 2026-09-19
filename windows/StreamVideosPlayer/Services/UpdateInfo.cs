using System.Text.Json.Serialization;

namespace StreamVideosPlayer.Services;

// Mirrors GET /windows-app/version in main.go. An empty Version means no
// release has been published yet (the server returns a bare "{}").
public class UpdateInfo
{
    [JsonPropertyName("version")]
    public string Version { get; set; } = "";

    [JsonPropertyName("url")]
    public string Url { get; set; } = "";

    [JsonPropertyName("notes")]
    public string Notes { get; set; } = "";
}
