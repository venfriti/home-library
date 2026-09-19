using System.IO;
using System.Text.Json;

namespace StreamVideosPlayer.Services;

// Unlike CredentialStore, nothing here is sensitive (just a UI preference),
// so this is plain JSON with no DPAPI encryption.
public class AppSettings
{
    public bool UseLocalSource { get; set; }

    private static readonly string FilePath = Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData),
        "StreamVideosPlayer", "settings.json");

    public static AppSettings Load()
    {
        try
        {
            if (!File.Exists(FilePath)) return new AppSettings();
            var json = File.ReadAllText(FilePath);
            return JsonSerializer.Deserialize<AppSettings>(json) ?? new AppSettings();
        }
        catch
        {
            return new AppSettings();
        }
    }

    public void Save()
    {
        try
        {
            Directory.CreateDirectory(Path.GetDirectoryName(FilePath)!);
            File.WriteAllText(FilePath, JsonSerializer.Serialize(this));
        }
        catch
        {
            // Best-effort: worst case the source toggle just doesn't
            // remember itself for next launch.
        }
    }
}
