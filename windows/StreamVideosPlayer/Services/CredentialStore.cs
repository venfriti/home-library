using System.IO;
using System.Security.Cryptography;
using System.Text;
using System.Text.Json;

namespace StreamVideosPlayer.Services;

public record StoredCredentials(string ServerUrl, string Password);

// Password is DPAPI-protected (CurrentUser scope) so the server's 7-day
// session (sessionTTL in main.go) can be silently renewed on relaunch
// instead of prompting for a password every week. DPAPI ties the ciphertext
// to this Windows account on this machine, so the file is useless if copied
// elsewhere.
public static class CredentialStore
{
    private static readonly string Dir = Path.Combine(
        Environment.GetFolderPath(Environment.SpecialFolder.ApplicationData),
        "StreamVideosPlayer");

    private static readonly string FilePath = Path.Combine(Dir, "credentials.dat");

    public static StoredCredentials? Load()
    {
        try
        {
            if (!File.Exists(FilePath)) return null;
            var protectedBytes = File.ReadAllBytes(FilePath);
            var json = ProtectedData.Unprotect(protectedBytes, null, DataProtectionScope.CurrentUser);
            return JsonSerializer.Deserialize<StoredCredentials>(Encoding.UTF8.GetString(json));
        }
        catch
        {
            // Corrupt, missing, or written by a different Windows account --
            // treat it the same as "no saved credentials" and fall back to
            // an interactive login instead of crashing on startup.
            return null;
        }
    }

    public static void Save(StoredCredentials creds)
    {
        Directory.CreateDirectory(Dir);
        var json = JsonSerializer.SerializeToUtf8Bytes(creds);
        var protectedBytes = ProtectedData.Protect(json, null, DataProtectionScope.CurrentUser);
        File.WriteAllBytes(FilePath, protectedBytes);
    }

    public static void Clear()
    {
        try { File.Delete(FilePath); } catch { /* already gone */ }
    }
}
