# Third-party iOS engine artifacts

> **License / GPL corresponding source.** sing-box is **GPL-3.0-or-later** and
> `Libbox.xcframework` is statically linked into the app and the PacketTunnel
> extension, so the whole iOS app is GPL-3.0-or-later (see the repo `LICENSE`
> and `THIRD_PARTY_NOTICES.md`). The clone below uses `--branch testing`, a
> moving target — for each released build, **pin and record the exact sing-box
> commit SHA** you compiled so the GPL §6 corresponding source is reproducible.
>
> **App Store caveat:** distributing this GPL-linked binary through the App
> Store (and likely external TestFlight) conflicts with Apple's Usage Rules /
> DRM under GPL §6/§10. OpenRung cannot resolve this for the sing-box portion
> alone — resolve it before any public App Store release (exception from
> SagerNet, or move to an out-of-process engine).

`Libbox.xcframework` is generated locally from sing-box and intentionally ignored by git because it is large.

To rebuild it:

```sh
git clone --depth 1 --branch testing https://github.com/SagerNet/sing-box.git /private/tmp/openrung-sing-box
cd /private/tmp/openrung-sing-box
go install github.com/sagernet/gomobile/cmd/gomobile@v0.1.12
go install github.com/sagernet/gomobile/cmd/gobind@v0.1.12
PATH="$HOME/go/bin:$PATH" gomobile init
PATH="$HOME/go/bin:$PATH" go run ./cmd/internal/build_libbox -target apple -platform ios,iossimulator -debug
mkdir -p /Users/.../Documents/OpenRung/ios/ThirdParty
cp -R Libbox.xcframework /Users/.../Documents/OpenRung/ios/ThirdParty/Libbox.xcframework
```
