// Guards the packaging acceptance check itself: a verifier that accepted
// anything would leave the very builds it exists to catch — no with_utls, a
// stale engine, an unstamped app version — shipping green.
import assert from 'node:assert/strict';
import test from 'node:test';

import { embedsDriver, verifyVersionOutput } from './verify-bundled-engine.mjs';

const good = 'OpenRung/0.1.5\nsing-box/1.14.0-beta.17 (bundled, with_utls)\n';

test('accepts the version output of a correctly built package', () => {
  assert.equal(
    verifyVersionOutput(good, '0.1.5', '1.14.0-beta.17'),
    'sing-box/1.14.0-beta.17 (bundled, with_utls)',
  );
  // Windows line endings and a GUI binary's trailing newline are not drift.
  assert.equal(
    verifyVersionOutput(good.replace(/\n/g, '\r\n'), '0.1.5', '1.14.0-beta.17'),
    'sing-box/1.14.0-beta.17 (bundled, with_utls)',
  );
});

test('rejects a build that lost the with_utls tag', () => {
  const untagged =
    'OpenRung/0.1.5\nsing-box/1.14.0-beta.17 (bundled, NO with_utls — cannot dial Reality relays; rebuild with -tags with_utls)';
  assert.throws(() => verifyVersionOutput(untagged, '0.1.5', '1.14.0-beta.17'), /with_utls/);
});

test('rejects an engine version other than the resolved module version', () => {
  assert.throws(
    () => verifyVersionOutput(good, '0.1.5', '1.14.0-beta.18'),
    /want "sing-box\/1\.14\.0-beta\.18 \(bundled, with_utls\)"/,
  );
});

test('rejects a package whose app version was never stamped in', () => {
  const unstamped = 'OpenRung/dev\nsing-box/1.14.0-beta.17 (bundled, with_utls)';
  assert.throws(() => verifyVersionOutput(unstamped, '0.1.5', '1.14.0-beta.17'), /OpenRung\/dev/);
});

test('rejects output missing the engine line entirely', () => {
  assert.throws(() => verifyVersionOutput('OpenRung/0.1.5', '0.1.5', '1.14.0-beta.17'), /undefined/);
});

// The WinDivert gate compares content, because a lost build tag shows up only
// as the driver's bytes reappearing inside the executable. Verified against
// real cross-compiled binaries: a with_external_windivert build has none of
// these bytes, an otherwise identical build without the tag has them all.
test('detects embedded driver bytes anywhere in a binary', () => {
  const driver = Buffer.alloc(8192, 0xab);
  driver.write('WinDivert', 0);
  const probe = driver.subarray(0, 4096);

  const clean = Buffer.concat([Buffer.alloc(1024, 0x01), Buffer.alloc(1024, 0x02)]);
  assert.equal(embedsDriver(clean, driver), false);

  const embedded = Buffer.concat([Buffer.alloc(1024, 0x01), probe, Buffer.alloc(1024, 0x02)]);
  assert.equal(embedsDriver(embedded, driver), true);

  // Only the leading probe is searched for, so a binary carrying a later
  // slice of the driver but not its head is not a match — the driver is
  // always embedded whole, head first.
  const tail = Buffer.concat([Buffer.alloc(16, 0x01), driver.subarray(4096)]);
  assert.equal(embedsDriver(tail, driver), false);
});
