[Setup]
AppName=NAS Stack
AppVersion=1.0.0-beta
DefaultDirName={autopf}\NAS Stack
OutputDir=..\..\build
OutputBaseFilename=nas-stack-1.0.0-beta-setup
Compression=lzma2
WizardStyle=modern
PrivilegesRequired=lowest

[Files]
Source: "..\..\scripts\install.ps1"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\scripts\install.sh"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\docs\*"; DestDir: "{app}\docs"; Flags: recursesubdirs
Source: "..\..\README.md"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\NAS Stack Setup"; Filename: "powershell.exe"; Parameters: "-ExecutionPolicy Bypass -File ""{app}\scripts\install.ps1"""

[Run]
Filename: "powershell.exe"; Parameters: "-ExecutionPolicy Bypass -File ""{app}\scripts\install.ps1"""; Description: "Запустить установку"; Flags: postinstall shellexec
