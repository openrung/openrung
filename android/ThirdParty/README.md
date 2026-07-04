# Third-party Android engine artifacts

> **License / GPL corresponding source.** sing-box is **GPL-3.0-or-later**, and
> `libbox.aar` is statically linked into the APK, so the whole app is
> GPL-3.0-or-later (see the repo `LICENSE` and `THIRD_PARTY_NOTICES.md`). The
> clone below uses `--branch testing`, which is a moving target. For each
> released build, **pin and record the exact sing-box commit SHA** you compiled
> (replace `--branch testing` with `--branch <sha>` or add `git checkout <sha>`)
> so the GPL §6 corresponding source for that binary is reproducible. This file
> is build instructions, **not** the attribution file — attribution lives in
> the repo-root `THIRD_PARTY_NOTICES.md`.

The Android client expects a local generated sing-box/libbox AAR at:

```text
android/app/libs/libbox.aar
```

Build the pinned release-mode AAR with:

```sh
cd android
./build-libbox-release.sh
```

The AAR is intentionally ignored by Git because it is generated and large.

The iOS client already uses the same local-artifact pattern for `ios/ThirdParty/Libbox.xcframework`.

## Build direction

Use sing-box's Android libbox build flow, then copy the generated AAR into:

```sh
mkdir -p android/app/libs
cp /path/to/generated/libbox.aar android/app/libs/libbox.aar
```

The local emulator build was validated with:

```sh
brew install openjdk@17 android-commandlinetools
JAVA_HOME=/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home \
  sdkmanager --sdk_root=/Users/.../Library/Android/sdk 'ndk;29.0.14206865'

go install github.com/sagernet/gomobile/cmd/gomobile@v0.1.12
go install github.com/sagernet/gomobile/cmd/gobind@v0.1.12

ANDROID_HOME="$HOME/Library/Android/sdk" \
ANDROID_NDK_HOME="$HOME/Library/Android/sdk/ndk/29.0.14206865" \
JAVA_HOME=/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home \
PATH="$HOME/go/bin:/opt/homebrew/opt/openjdk@17/bin:$PATH" \
  gomobile init

git clone --depth 1 --branch testing https://github.com/SagerNet/sing-box.git /private/tmp/openrung-sing-box-android
cd /private/tmp/openrung-sing-box-android
ANDROID_HOME="$HOME/Library/Android/sdk" \
ANDROID_NDK_HOME="$HOME/Library/Android/sdk/ndk/29.0.14206865" \
JAVA_HOME=/opt/homebrew/opt/openjdk@17/libexec/openjdk.jdk/Contents/Home \
PATH="$HOME/go/bin:/opt/homebrew/opt/openjdk@17/bin:$PATH" \
  go run ./cmd/internal/build_libbox -target android -platform android/arm64

cp /private/tmp/openrung-sing-box-android/libbox.aar android/app/libs/libbox.aar
```

Do not pass `-debug` for distributed APKs. The release-mode libbox build uses
Go's `-trimpath` and strips debug symbols so local source paths are not embedded
in `libbox.so`.

## Signed release APK

Release signing is read from environment variables and is never committed:

- `OPENRUNG_RELEASE_STORE_FILE`
- `OPENRUNG_RELEASE_STORE_PASSWORD`
- `OPENRUNG_RELEASE_KEY_ALIAS` (defaults to `openrung`)
- `OPENRUNG_RELEASE_KEY_PASSWORD`

On the primary macOS build machine, `./build-release.sh` reads the password from
the `org.openrung.android.release` Keychain entry and builds the signed APK. Back
up both the release keystore and its password: losing them prevents future APKs
from upgrading existing release installations.

`ndk;27.3.13750724` failed locally while linking sing-box's prebuilt Cronet library with `unknown relocation (315)`. `ndk;29.0.14206865` built the arm64 emulator AAR successfully.

After the AAR is available, `LibboxProxyEngine` uses the generated Android API with:

- the selected OpenRung relay,
- the generated sing-box JSON config,
- the active `VpnService` for socket protection and lifecycle callbacks,
- a minimal `PlatformInterface` implementation that opens the Android VPN TUN when libbox starts.
