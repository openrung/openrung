# Build the Windows app, packaged as a zip. The app statically links the
# sing-box engine (internal/singboxruntime) and runs it by re-invoking its own
# .exe, so the package is one executable plus its notices — no sing-box to
# install, bundle, or find on PATH.
#
# Prereqs: Go, Node >=22, the Wails CLI, and the WebView2 runtime (present on
# Windows 10/11 by default; the NSIS installer also bootstraps it).
$ErrorActionPreference = 'Stop'
Set-Location (Join-Path $PSScriptRoot '..')

$env:PATH = "$env:PATH;$(go env GOPATH)\bin"
& node scripts/versioned-wails-build.mjs @args
if ($LASTEXITCODE -ne 0) { throw "wails build failed with exit code $LASTEXITCODE" }

$exe = 'build\bin\OpenRung.exe'
if (-not (Test-Path $exe)) { Write-Error "$exe not found after build"; exit 1 }

# Refuses to package a build that lost a sing-box build tag or the app-version
# stamp; also yields the engine version line for the notices below.
$sbVer = & node scripts/verify-bundled-engine.mjs $exe
if ($LASTEXITCODE -ne 0) { throw "bundled engine verification failed with exit code $LASTEXITCODE" }
Write-Host "==> verified bundled engine: $sbVer"

$stage = 'build\OpenRung'
if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Path $stage | Out-Null
Copy-Item $exe (Join-Path $stage 'OpenRung.exe')

# The full repository notices and the GPL text must travel with the package:
# the linked engine embeds the wintun.dll driver binary, whose license (the
# Wintun Prebuilt Binaries License) THIRD_PARTY_NOTICES.md section 8 /
# Appendix B carries. WinDivert is not in this binary — every build passes
# with_external_windivert (scripts/versioned-wails-build.mjs).
Copy-Item ..\THIRD_PARTY_NOTICES.md (Join-Path $stage 'THIRD_PARTY_NOTICES.md')
Copy-Item ..\LICENSE (Join-Path $stage 'LICENSE')

@"
This application statically links $sbVer, licensed GPL-3.0-or-later
(text: LICENSE). The Windows build embeds the wintun.dll driver binary; its
notice and license text are in THIRD_PARTY_NOTICES.md (section 8 and
Appendix B) alongside this file. OpenRung is free software
(GPL-3.0-or-later): https://github.com/openrung/openrung
"@ | Set-Content (Join-Path $stage 'THIRD_PARTY_NOTICES.txt')

$out = 'build\bin\OpenRung-windows-amd64.zip'
if (Test-Path $out) { Remove-Item $out }
Compress-Archive -Path "$stage\*" -DestinationPath $out
Write-Host "==> done: $out"
