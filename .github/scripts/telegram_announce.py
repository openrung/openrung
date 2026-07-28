#!/usr/bin/env python3
"""Announce a release in the OpenRung Telegram channels.

CANONICAL COPY: openrung/openrung-telegram-bot (private), at
deploy/github-actions/. Deployed copies live in the public release repos at
.github/scripts/telegram_announce.py — keep all copies in sync.

The script never invents copy. It posts exactly the per-language text an
author wrote, because these channels serve people in Russia, Iran and China
who are making risk decisions: machine-mangled Farsi or an over-promising
claim costs trust that is very hard to win back. Announcement copy comes from
one of two places, checked in this order:

  1. An HTML comment block in the release body (hand-written release notes —
     how the desktop repo publishes):

         <!--telegram
         [en]
         🔄 <b>OpenRung Desktop 0.1.4</b>
         • Something a user can feel
         [ru]
         ...
         [fa]
         ...
         [zh]
         ...
         -->

  2. A committed file, .github/telegram/<tag>.md, holding the same
     [lang] sections without the surrounding comment (for repos whose release
     notes are CI-generated, e.g. the mobile app: commit the copy in the
     version-bump PR and it is present at the tag when the release fires).

Text is sent with Telegram's HTML parse mode, so sections may use <b>, <i>,
<code> and <a href>. Bare "&", "<" and ">" must be escaped as &amp; &lt; &gt;
or Telegram rejects the message (a loud, visible CI failure — not a silent
mangled post).

Preamble directives may appear before the first [lang] section:

    preview = on     # keep link previews (use when the preview IS the point,
                     # e.g. an Apple TestFlight card); default is off

To deliberately ship a release with no announcement (internal or hotfix
builds), put <!--telegram:skip--> in the release body or commit
.github/telegram/<tag>.skip. Silence must be explicit: with neither copy nor
skip marker the job FAILS, which is what stops releases going out unannounced.

Environment:
    TELEGRAM_BOT_TOKEN  bot credential (required unless DRY_RUN=true)
    RELEASE_TAG         e.g. v0.3.4                             (required)
    RELEASE_BODY        release notes; may be empty             (optional)
    DRY_RUN             "true" renders and validates, sends nothing
    GITHUB_STEP_SUMMARY path for the run summary                (optional)
"""

from __future__ import annotations

import json
import os
import re
import sys
import time
import urllib.error
import urllib.request

# Public channel handles. Language -> channel.
CHANNELS = {
    "en": "@openrung_official",
    "ru": "@openrung_ru",
    "fa": "@openrung_fa",
    "zh": "@openrung_zh",
}

API_BASE = os.environ.get("TELEGRAM_API_BASE", "https://api.telegram.org")

BLOCK_RE = re.compile(r"<!--\s*telegram\s*\n(.*?)-->", re.S | re.I)
SKIP_RE = re.compile(r"<!--\s*telegram:skip\s*-->", re.I)
SECTION_RE = re.compile(r"^\[(en|ru|fa|zh)\][ \t]*$", re.I | re.M)
PREVIEW_RE = re.compile(r"^\s*preview\s*=\s*(on|off)\s*$", re.I | re.M)

TEMPLATE = """<!--telegram
[en]
\U0001F504 <b>OpenRung ... </b>
• What changed, in terms a user can feel

[ru]
...

[fa]
...

[zh]
...
-->"""


class AnnounceError(RuntimeError):
    """Fatal, actionable problem — reported and exits non-zero."""


def log(msg: str) -> None:
    print(msg, flush=True)


def notice(msg: str) -> None:
    log(f"::notice::{msg}")


def warn(msg: str) -> None:
    log(f"::warning::{msg}")


def parse_sections(block: str) -> tuple[dict[str, str], bool]:
    """Split a block into {lang: text} plus the link-preview setting."""
    preamble = SECTION_RE.split(block, maxsplit=1)[0]
    preview_match = PREVIEW_RE.search(preamble)
    preview = bool(preview_match) and preview_match.group(1).lower() == "on"

    parts = SECTION_RE.split(block)
    # parts == [preamble, lang, body, lang, body, ...]
    sections: dict[str, str] = {}
    for lang, body in zip(parts[1::2], parts[2::2]):
        lang = lang.lower()
        body = body.strip()
        if not body:
            continue
        if lang in sections:
            raise AnnounceError(f"duplicate [{lang}] section in the announcement block")
        sections[lang] = body
    return sections, preview


def load_copy(tag: str, body: str) -> tuple[dict[str, str], bool, str]:
    """Find announcement copy in the release body, else in the committed file."""
    match = BLOCK_RE.search(body)
    if match:
        sections, preview = parse_sections(match.group(1))
        if sections:
            return sections, preview, "release body"
        raise AnnounceError(
            "found a <!--telegram--> block in the release body but no [en]/[ru]/[fa]/[zh] sections"
        )

    path = os.path.join(".github", "telegram", f"{tag}.md")
    if os.path.exists(path):
        with open(path, encoding="utf-8") as fh:
            sections, preview = parse_sections(fh.read())
        if not sections:
            raise AnnounceError(f"{path} contains no [en]/[ru]/[fa]/[zh] sections")
        return sections, preview, path

    raise AnnounceError(
        f"no announcement copy for {tag}.\n"
        f"Add a block to the release notes:\n\n{TEMPLATE}\n\n"
        f"or commit {path} with the same [lang] sections (no comment wrapper).\n"
        "Deliberately silent release? Put <!--telegram:skip--> in the release body "
        f"or commit .github/telegram/{tag}.skip"
    )


def skip_requested(tag: str, body: str) -> bool:
    return bool(SKIP_RE.search(body)) or os.path.exists(
        os.path.join(".github", "telegram", f"{tag}.skip")
    )


def call(token: str, method: str, payload: dict) -> dict:
    """POST one Bot API method, retrying once on 429.

    Errors are scrubbed of the token: this runs in CI, where anything printed
    is retained in logs that outlive the run.
    """
    req = urllib.request.Request(
        f"{API_BASE}/bot{token}/{method}",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
    )
    for attempt in (1, 2):
        try:
            with urllib.request.urlopen(req, timeout=30) as resp:
                result = json.load(resp)
            if result.get("ok"):
                return result["result"]
            retry_after = (result.get("parameters") or {}).get("retry_after")
            if retry_after and attempt == 1:
                time.sleep(min(int(retry_after), 30))
                continue
            raise AnnounceError(
                f"{method} rejected by Telegram: "
                f"{result.get('error_code')} {result.get('description')}"
            )
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", "replace")[:400]
            if exc.code == 429 and attempt == 1:
                time.sleep(5)
                continue
            raise AnnounceError(
                f"{method} HTTP {exc.code}: {detail.replace(token, '<token>')}"
            ) from None
        except urllib.error.URLError as exc:
            raise AnnounceError(
                f"{method} network error: {str(exc.reason).replace(token, '<token>')}"
            ) from None
    raise AnnounceError(f"{method} failed after retry")


def summarize(rows: list[str], tag: str, source: str, dry_run: bool) -> None:
    path = os.environ.get("GITHUB_STEP_SUMMARY")
    if not path:
        return
    head = "Dry run — nothing sent" if dry_run else "Posted"
    with open(path, "a", encoding="utf-8") as fh:
        fh.write(f"### Telegram announcement — {tag}\n\n")
        fh.write(f"{head}. Copy source: `{source}`\n\n")
        fh.write("| Language | Channel | Message |\n|---|---|---|\n")
        fh.write("\n".join(rows) + "\n")


def main() -> int:
    tag = os.environ.get("RELEASE_TAG", "").strip()
    if not tag:
        raise AnnounceError("RELEASE_TAG is empty")
    body = os.environ.get("RELEASE_BODY", "")
    dry_run = os.environ.get("DRY_RUN", "").lower() == "true"

    if skip_requested(tag, body):
        notice(f"{tag}: announcement explicitly skipped — nothing posted.")
        return 0

    sections, preview, source = load_copy(tag, body)
    notice(f"{tag}: announcement copy from {source} ({', '.join(sorted(sections))}).")

    missing = [lang for lang in CHANNELS if lang not in sections]
    if missing:
        # Posting some languages beats posting none, but a channel silently
        # missing its release note is worth shouting about.
        warn(
            f"{tag}: no copy for {', '.join(missing)} — "
            f"{', '.join(CHANNELS[m] for m in missing)} will not be posted."
        )

    token = os.environ.get("TELEGRAM_BOT_TOKEN", "")
    if not token and not dry_run:
        raise AnnounceError("TELEGRAM_BOT_TOKEN is empty (set the repo secret)")

    rows: list[str] = []
    for lang, channel in CHANNELS.items():
        text = sections.get(lang)
        if not text:
            continue
        if dry_run:
            log(f"\n--- {channel} ({lang}) ---\n{text}\n")
            rows.append(f"| {lang} | {channel} | _dry run_ |")
            continue
        payload = {
            "chat_id": channel,
            "text": text,
            "parse_mode": "HTML",
            "link_preview_options": {"is_disabled": not preview},
        }
        msg = call(token, "sendMessage", payload)
        link = f"https://t.me/{channel.lstrip('@')}/{msg['message_id']}"
        log(f"{channel}: posted -> {link}")
        rows.append(f"| {lang} | {channel} | [{msg['message_id']}]({link}) |")
        time.sleep(0.5)  # be gentle; nowhere near Telegram's limits

    summarize(rows, tag, source, dry_run)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except AnnounceError as err:
        log(f"::error::{err}")
        sys.exit(1)
