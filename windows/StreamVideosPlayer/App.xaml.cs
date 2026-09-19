using System.Windows;
using LibVLCSharp.Shared;
using StreamVideosPlayer.Services;

namespace StreamVideosPlayer;

public partial class App : Application
{
    protected override async void OnStartup(StartupEventArgs e)
    {
        base.OnStartup(e);
        Core.Initialize();

        var creds = CredentialStore.Load();
        if (creds is not null)
        {
            var client = new ServerClient(creds.ServerUrl);
            try
            {
                if (await client.LoginAsync(creds.Password))
                {
                    new MainWindow(client, creds).Show();
                    return;
                }
            }
            catch
            {
                // Server unreachable, bad URL, TLS issue, etc. -- fall through
                // to a normal login prompt instead of failing silently.
            }
        }

        new LoginWindow().Show();
    }
}
