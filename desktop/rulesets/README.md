# Bundled desktop split-tunneling rule sets

`dist/` contains the four compiled sing-box rule-set binaries behind the
desktop proxy client's Iranian and Chinese bypass presets. The Go package in
this directory embeds all four files, stages them atomically to an on-disk
directory, and enables a country only when both its `geosite` and `geoip` files
match the pinned hashes below. Missing or damaged data drops that preset so its
traffic stays on the proxy; it never prevents a connection.

Do not edit the binaries in place. Refreshes must replace the files, update this
table, and validate every file with the sing-box revision shipped by desktop.

## Provenance

| File | Source repository | Branch @ commit | Fetched | SHA-256 | License |
| --- | --- | --- | --- | --- | --- |
| `geosite-ir.srs` | [Chocolate4U/Iran-sing-box-rules](https://github.com/Chocolate4U/Iran-sing-box-rules) | `rule-set` @ `ef8d0d7afead` | 2026-07-22 | `22add255a0ea2fccc799a0c45508df5b67319d9d2c30ed2ad37bfa4e6d67ce81` | GPL-3.0 |
| `geoip-ir.srs` | [Chocolate4U/Iran-sing-box-rules](https://github.com/Chocolate4U/Iran-sing-box-rules) | `rule-set` @ `ef8d0d7afead` | 2026-07-22 | `36d46ea40dfe65d722ee4a4171bc93db8ad6f5dd75265ffb448979761ece9c53` | GPL-3.0 |
| `geosite-cn.srs` | [SagerNet/sing-geosite](https://github.com/SagerNet/sing-geosite) | `rule-set` @ `63c859070624` | 2026-07-22 | `a0dba9663dd160836106740198ed2ce78aa348946e50e5f5666e9a8b7c4097e4` | MIT (data from [v2fly/domain-list-community](https://github.com/v2fly/domain-list-community)) |
| `geoip-cn.srs` | [SagerNet/sing-geoip](https://github.com/SagerNet/sing-geoip) | `rule-set` @ `5605651c12ed` | 2026-07-12 | `bc1a9eb66f9c6a0fe9fc5300cf5b5e885e0f9eadd7213b085b767a95d6af3d2a` | MaxMind GeoLite2 data (attribution required) |

The GPL-3.0, MIT, and MaxMind attribution obligations are carried in the root
`THIRD_PARTY_NOTICES.md` and the desktop app's in-product license notices.

## Refreshing and validation

Fetch replacements from each upstream's `rule-set` branch, record the branch
head and fetch date, and update hashes with:

```sh
shasum -a 256 desktop/rulesets/dist/*.srs
```

Every file must decompile with the exact sing-box CLI revision bundled by the
desktop release before it is committed:

```sh
sing-box rule-set decompile <file> -o <out.json>
```

A rule set the shipped engine cannot decompile must not be released.
