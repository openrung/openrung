// Guards the desktop packaging scripts' license-notices contract: every
// staged desktop package must carry the repository's full
// THIRD_PARTY_NOTICES.md and the GPL text (LICENSE), because the Windows
// sing-box.exe embeds the wintun.dll and WinDivert64.sys driver binaries
// whose license texts live there (section 8 / Appendix B) — a hand-written
// summary .txt is not a substitute. The scripts only run on their native
// OSes, so this is a static contract check on the script sources, in the
// style of version-injection.test.mjs.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const read = (rel) => readFileSync(fileURLToPath(new URL(rel, import.meta.url)), 'utf8');

const packagingScripts = ['package-windows.ps1', 'package-linux.sh', 'package-macos.sh'];

for (const script of packagingScripts) {
  test(`${script} stages the full repository notices and the GPL text`, () => {
    const source = read(`./${script}`);
    // Both files must be copied from the repo root (one level above
    // desktop/, the scripts' working directory) into the stage.
    assert.match(
      source,
      /(cp|Copy-Item) \.\.[\\/]THIRD_PARTY_NOTICES\.md/,
      `${script} must copy the repository THIRD_PARTY_NOTICES.md into the package stage`,
    );
    assert.match(
      source,
      /(cp|Copy-Item) \.\.[\\/]LICENSE/,
      `${script} must copy the repository LICENSE (GPL-3.0 text) into the package stage`,
    );
  });
}

test('the repository notices carry the Windows driver licenses the scripts rely on', () => {
  const notices = read('../../THIRD_PARTY_NOTICES.md');
  // Section 8 documents both drivers embedded in the Windows sing-box.exe...
  assert.ok(notices.includes('wintun.dll'), 'THIRD_PARTY_NOTICES.md must cover wintun.dll');
  assert.ok(notices.includes('WinDivert64.sys'), 'THIRD_PARTY_NOTICES.md must cover WinDivert');
  // ...Appendix B reproduces the Wintun Prebuilt Binaries License verbatim
  // (its terms must accompany the binary), through its final section.
  assert.ok(
    notices.includes('Wintun Prebuilt Binaries License'),
    'THIRD_PARTY_NOTICES.md must name the Wintun Prebuilt Binaries License',
  );
  assert.ok(
    notices.includes('RESERVATION OF RIGHTS'),
    'THIRD_PARTY_NOTICES.md must reproduce the Wintun license text in full (Appendix B)',
  );
});
