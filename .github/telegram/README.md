# Telegram release announcements

Copy for the four OpenRung Telegram channels (`@openrung_official`,
`@openrung_ru`, `@openrung_fa`, `@openrung_zh`), posted by
[`telegram-announce.yml`](../workflows/telegram-announce.yml) when a release is
published.

Put the copy in **either** place:

- the release notes, wrapped in `<!--telegram ... -->` (preferred here: this
  repo's release notes are hand-written), or
- `<tag>.md` in this directory, using the same `[lang]` sections without the
  comment wrapper (for releases whose notes are CI-generated).

```
<!--telegram
[en]
🔄 <b>OpenRung Desktop 0.1.4</b>
• What changed, in terms a user can feel

[ru]
…
[fa]
…
[zh]
…
-->
```

Text is sent with Telegram's HTML parse mode: `<b>`, `<i>`, `<code>` and
`<a href>` work; bare `&`, `<` and `>` must be escaped or Telegram rejects the
message and the job fails. Put `preview = on` above the first section to keep
link previews (useful when the preview itself is the point, like an Apple
TestFlight card).

No announcement for a release? Say so explicitly with `<!--telegram:skip-->` in
the release body or an empty `<tag>.skip` file here — otherwise the job fails,
which is what stops releases shipping unannounced.

Translations are written by hand, never machine-generated: these channels reach
people making risk decisions about censorship circumvention, and mangled Farsi
or an over-promising claim costs trust that is hard to win back. Write the
English, then ask for the RU/FA/ZH versions.
