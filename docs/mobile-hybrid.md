# Hybrid Mobile Client Direction

OpenRung should use a hybrid mobile architecture: shared cross-platform UI later, native VPN transport always.

## Decision

Use native iOS and Android VPN implementations for the network path, with a React Native shell as the likely cross-platform control app once the native tunnel path is proven.

React Native is a good fit for:

- onboarding,
- broker settings,
- connect and disconnect controls,
- relay status,
- diagnostics,
- account and abuse-reporting flows later.

Native code remains responsible for:

- iOS `NEPacketTunnelProvider`,
- Android `VpnService`,
- TUN packet handling,
- DNS behavior,
- embedded Xray-compatible engine lifecycle,
- fail-closed behavior,
- OS-specific VPN permission and background lifecycle edge cases.

## Recommended sequence

1. Finish the iOS native tunnel path.
2. Verify the embedded VLESS Reality Vision engine on a signed real iPhone.
3. Verify full-device routing and DNS behavior on that device.
4. Build the Android native tunnel path.
5. Add a React Native shell that calls native modules for VPN actions and status.

## Native module contract for the future React Native shell

The future React Native app should only need a small bridge:

```text
configureBroker(url)
prepareVPN()
connect()
disconnect()
getStatus()
getLastError()
getCurrentRelay()
```

The bridge should not expose packet-level or engine-level details to React Native.

## Current repo state

The iOS scaffold in `ios/` already follows this split. The SwiftUI host app is a temporary initial UI, and the packet tunnel now owns the sing-box/libbox engine behind a small Swift boundary that can later sit behind a React Native native module.
