# Build the Windows app with sing-box bundled next to the .exe, packaged as a
# zip. Run this ON Windows (Wails cannot cross-compile from macOS/Linux).
#
# Prereqs: Go, Node >=22, the Wails CLI, and the WebView2 runtime (present on
# Windows 10/11 by default; the NSIS installer also bootstraps it).
#
# Provide a Windows sing-box.exe via $env:SING_BOX = 'C:\path\to\sing-box.exe',
# or have sing-box on PATH.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

$singbox = $env:SING_BOX
if (-not $singbox) { $cmd = Get-Command sing-box -ErrorAction SilentlyContinue; if ($cmd) { $singbox = $cmd.Source } }
if (-not $singbox -or -not (Test-Path $singbox)) {
    Write-Error "no sing-box. Set `$env:SING_BOX to a Windows sing-box.exe or install it on PATH."
    exit 1
}

$env:PATH = "$env:PATH;$(go env GOPATH)\bin"
Write-Host "==> wails build $args"
wails build @args

$exe = 'build\bin\OpenRung.exe'
if (-not (Test-Path $exe)) { Write-Error "$exe not found after build"; exit 1 }

$stage = 'build\OpenRung'
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Path $stage | Out-Null
Copy-Item $exe (Join-Path $stage 'OpenRung.exe')
Copy-Item $singbox (Join-Path $stage 'sing-box.exe')   # resolver finds it next to the .exe

$sbVer = (& $singbox version 2>$null | Select-Object -First 1)
@"
This application bundles sing-box ($sbVer), licensed under GPL-3.0.
Source: https://github.com/SagerNet/sing-box
OpenRung is free software (GPL-3.0-or-later): https://github.com/openrung/openrung
"@ | Set-Content (Join-Path $stage 'THIRD_PARTY_NOTICES.txt')

$out = 'build\bin\OpenRung-windows-amd64.zip'
if (Test-Path $out) { Remove-Item $out }
Compress-Archive -Path "$stage\*" -DestinationPath $out
Write-Host "==> done: $out"