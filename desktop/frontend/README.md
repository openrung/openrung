# Desktop frontend

The desktop client's interface is the mobile app's. This package compiles the
React Native source of `openrung-mobile-app` for the Wails webview with
react-native-web, so the two apps cannot drift apart. It contains no screens
of its own.

## How it fits together

- **UI source**: `../mobile-app`, a submodule pinned to an
  `openrung-mobile-app` commit. Fetch it with `git submodule update --init`.
  To change a screen, change it in that repository and move the pin.
- **`react-native`** resolves to react-native-web (`src/shims/reactNative.ts`).
- **Native-only libraries** are replaced by web implementations of the same
  API in `src/shims/`: the MapLibre map (on `maplibre-gl`) and the iOS tab bar.
- **The native bridge** (`src/native/installNativeModules.ts`) backs the app's
  `OpenRungVpn` / `OpenRungBroker` / `OpenRungDesktop` module contract
  (`docs/CONTRACT.md` in the mobile repository) with the Wails-bound Go
  service in `desktop/vpnservice`.
- **License notices** (`src/licenses/notices.ts`) replace the mobile app's
  inventory at build time: the screen must list what this build ships.

`vite.config.ts` wires these up and compiles the mobile TypeScript itself,
because that tree's `tsconfig.json` depends on packages installed only in the
mobile repository.

## Commands

```sh
npm ci
npm test         # bridge, shims and license-notice checks
npm run build    # type-check and bundle into dist/, which the Go binary embeds
npm run dev      # http://localhost:5173
```

`npm run dev` on its own is a browser preview with no Go service: the app
falls back to its scripted mock, and the relay directory and proxy details
come from fixtures. `wails dev` from `desktop/` runs the same dev server
inside the real shell.

## Developing against a mobile checkout

To try unmerged mobile changes, point the build at another checkout instead
of the submodule, in the environment or in an untracked `.env.local`:

```sh
OPENRUNG_MOBILE_APP=/path/to/openrung-mobile-app
```

Release builds must use the submodule.
