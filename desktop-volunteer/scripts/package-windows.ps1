# Build the Windows app with xray bundled next to the .exe, packaged as a
# zip. Run this ON Windows (Wails cannot cross-compile from macOS/Linux).
#
# Prereqs: Go, Node >=22, the Wails CLI, and the WebView2 runtime (present on
# Windows 10/11 by default; the NSIS installer also bootstraps it).
#
# Provide a Windows xray.exe via $env:XRAY = 'C:\path\to\xray.exe',
# or have xray on PATH.
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

$xray = $env:XRAY
if (-not $xray) { $cmd = Get-Command xray -ErrorAction SilentlyContinue; if ($cmd) { $xray = $cmd.Source } }
if (-not $xray -or -not (Test-Path $xray)) {
    Write-Error "no xray. Set `$env:XRAY to a Windows xray.exe or install it on PATH."
    exit 1
}

$env:PATH = "$env:PATH;$(go env GOPATH)\bin"
Write-Host "==> wails build $args"
wails build @args

$exe = 'build\bin\OpenRungVolunteer.exe'
if (-not (Test-Path $exe)) { Write-Error "$exe not found after build"; exit 1 }

$stage = 'build\OpenRungVolunteer'
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Path $stage | Out-Null
Copy-Item $exe (Join-Path $stage 'OpenRungVolunteer.exe')
Copy-Item $xray (Join-Path $stage 'xray.exe')   # resolver finds it next to the .exe

$xrVer = (& $xray version 2>$null | Select-Object -First 1)
@"
This application bundles Xray-core ($xrVer), licensed under MPL-2.0.
It is included unmodified and runs as a separate process.
Source: https://github.com/XTLS/Xray-core
License text: https://www.mozilla.org/MPL/2.0/
OpenRung Volunteer is free software (GPL-3.0-or-later): https://github.com/openrung/openrung
"@ | Set-Content (Join-Path $stage 'THIRD_PARTY_NOTICES.txt')

# Full corresponding-source license texts (GPL-3.0-or-later requires the License
# to accompany every conveyed binary; MPL-2.0 requires Xray's). $env:XRAY_LICENSE
# (Xray's LICENSE from the release zip) is set by CI; skipped for a local build.
Copy-Item '..\LICENSE' (Join-Path $stage 'LICENSE.txt')
Copy-Item '..\THIRD_PARTY_NOTICES.md' (Join-Path $stage 'THIRD_PARTY_NOTICES.md')
if ($env:XRAY_LICENSE -and (Test-Path $env:XRAY_LICENSE)) {
    Copy-Item $env:XRAY_LICENSE (Join-Path $stage 'XRAY-LICENSE.txt')
}

$out = 'build\bin\OpenRungVolunteer-windows-amd64.zip'
if (Test-Path $out) { Remove-Item $out }
Compress-Archive -Path "$stage\*" -DestinationPath $out
Write-Host "==> done: $out"
