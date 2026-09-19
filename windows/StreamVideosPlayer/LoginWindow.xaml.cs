using System.Windows;
using System.Windows.Input;
using StreamVideosPlayer.Services;

namespace StreamVideosPlayer;

public partial class LoginWindow : Window
{
    // Once a blank-password attempt fails, we know this server actually
    // needs one (see isAuthed in main.go -- only non-LAN requests reach the
    // password check at all), so the field stays revealed for the rest of
    // this window's life instead of re-hiding on every retry.
    private bool _passwordRequired;

    public LoginWindow()
    {
        InitializeComponent();
    }

    private async void ConnectButton_Click(object sender, RoutedEventArgs e) => await TryConnectAsync();

    private async void PasswordBox_KeyDown(object sender, KeyEventArgs e)
    {
        if (e.Key == Key.Enter) await TryConnectAsync();
    }

    private async Task TryConnectAsync()
    {
        var serverUrl = ServerUrlBox.Text.Trim();
        if (string.IsNullOrWhiteSpace(serverUrl))
        {
            ShowError("Enter a server URL.");
            return;
        }

        ConnectButton.IsEnabled = false;
        ErrorText.Visibility = Visibility.Collapsed;

        try
        {
            var client = new ServerClient(serverUrl);
            var password = _passwordRequired ? PasswordBox.Password : "";

            if (await client.LoginAsync(password))
            {
                var creds = new StoredCredentials(serverUrl, password);
                CredentialStore.Save(creds);
                new MainWindow(client, creds).Show();
                Close();
                return;
            }

            if (!_passwordRequired)
            {
                // A blank password didn't get in -- this isn't a LAN-trusted
                // connection, so it genuinely needs one. Reveal the field
                // instead of reporting a plain failure.
                RevealPasswordField();
            }
            else
            {
                ShowError("Wrong password — check it and try again.");
            }
        }
        catch (Exception ex)
        {
            ShowError($"Couldn't reach the server: {ex.Message}");
        }
        finally
        {
            ConnectButton.IsEnabled = true;
        }
    }

    private void RevealPasswordField()
    {
        _passwordRequired = true;
        PasswordLabel.Visibility = Visibility.Visible;
        PasswordBox.Visibility = Visibility.Visible;
        ConnectButton.Content = "Sign in";
        PasswordBox.Focus();
    }

    private void ShowError(string message)
    {
        ErrorText.Text = message;
        ErrorText.Visibility = Visibility.Visible;
    }
}
