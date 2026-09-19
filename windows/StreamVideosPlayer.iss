; Inno Setup script for a proper Start Menu / uninstaller-backed installer.
; Not required to distribute the app -- a zipped self-contained publish
; (see windows/README.md) already runs standalone -- this is for a more
; polished handoff to non-technical users.
;
; Requires Inno Setup (https://jrsoftware.org/isinfo.php) and a prior
; self-contained publish:
;   dotnet publish -c Release -r win-x64 --self-contained true -o publish
; then compile this script (iscc StreamVideosPlayer.iss) from this directory.

#define MyAppName "Stream"
#define MyAppVersion "1.0.0"
#define MyAppPublisher "Venfriti"
#define MyAppExeName "StreamVideosPlayer.exe"

[Setup]
AppId={{B8B6C1B0-6C1E-4B7A-9C2E-9B8B6E5C7A11}
AppName={#MyAppName}
; Without this, Inno Setup defaults it to "{AppName} {AppVersion}" -- which is
; what was showing up in Control Panel as "Stream 1.0.0" instead of "Stream".
AppVerName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={autopf}\{#MyAppName}
DefaultGroupName={#MyAppName}
UninstallDisplayIcon={app}\{#MyAppExeName}
SetupIconFile=StreamVideosPlayer\app.ico
OutputBaseFilename=StreamVideosPlayer-Setup
Compression=lzma2
SolidCompression=yes
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

[Files]
Source: "StreamVideosPlayer\publish\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; Tasks: desktopicon

[Tasks]
Name: "desktopicon"; Description: "Create a desktop shortcut"; GroupDescription: "Additional shortcuts:"

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "Launch {#MyAppName}"; Flags: postinstall nowait skipifsilent
