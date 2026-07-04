# OpenRung Android MVP

This directory contains the native Android client for connecting a device to the OpenRung volunteer relay network.

## What is included

- Kotlin/Jetpack Compose app shell with a terminal-inspired interface.
- Visible broker URL field and one primary Connect/Disconnect button.
- `VpnService` lifecycle for Android VPN permission, foreground service, full-device routes, and disconnect handling.
- Broker relay fetch, relay filtering, and candidate ordering ported from the iOS client behavior.
- Sing-box VLESS Reality Vision config generation matching the Go and iOS clients.
- A `ProxyEngine` boundary for the embedded Android libbox/sing-box adapter.

## Current MVP boundary

The Android app owns the UI, VPN permission flow, broker fetch, relay selection, fail-closed lifecycle, and sing-box config generation.

The final packet bridge depends on a local Android libbox AAR generated from sing-box. Until that AAR and adapter are wired, the app will reach the engine-start phase and fail with a clear "Android libbox is not linked yet" error.

Expected data path once the Android libbox adapter is linked:

```text
Android apps
  -> OpenRungVpnService
  -> Android libbox / sing-box engine
  -> VLESS Reality Vision connection
  -> volunteer public host:port
  -> destination internet
```

## Build

Install Android Studio or the Android command-line SDK, then from this directory run:

```sh
./gradlew :app:assembleDebug
```

This repo does not currently check in a Gradle wrapper. If your machine has Gradle installed, you can run:

```sh
gradle :app:assembleDebug
```

## Test

```sh
gradle :app:testDebugUnitTest
```

## Android VPN notes

Android requires user approval before a `VpnService` can start. The app requests that approval when Connect is pressed. The service starts in the foreground, fetches relays from:

```http
GET /api/v1/relays?limit=5
```

It then tries usable relays in order and fails closed if none can start.
