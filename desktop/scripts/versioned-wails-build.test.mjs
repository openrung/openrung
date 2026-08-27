import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import {
  appVersionFromSources,
  mergedBuildTags,
  readAppVersion,
  singBoxBuildTags,
  versionedWailsBuildArgs,
} from './versioned-wails-build.mjs';

test('reads the app version from VERSION with a matching wails.json copy', () => {
  const directory = mkdtempSync(join(tmpdir(), 'openrung-version-'));
  const versionSource = join(directory, 'VERSION');
  const configSource = join(directory, 'wails.json');
  try {
    writeFileSync(versionSource, '4.5.6\n');
    writeFileSync(configSource, JSON.stringify({ info: { productVersion: '4.5.6' } }));
    assert.equal(readAppVersion(versionSource, configSource), '4.5.6');
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test('the checked-in VERSION and wails.json agree', () => {
  // Defaults read desktop/VERSION and desktop/wails.json; this is the PR-time
  // drift gate for the real files, not fixtures.
  assert.match(readAppVersion(), /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/);
});

test('accepts only canonical X.Y.Z versions from VERSION', () => {
  const config = { info: { productVersion: '0.1.4' } };
  assert.equal(appVersionFromSources('0.1.4\n', config), '0.1.4');

  for (const contents of [undefined, '', 'dev', 'v0.1.4', '0.1', '0.1.4-beta.1', '01.2.3']) {
    assert.throws(
      () => appVersionFromSources(contents, config),
      /must contain a semantic X\.Y\.Z version/,
    );
  }
});

test('rejects drift between VERSION and the wails.json copy', () => {
  assert.throws(
    () => appVersionFromSources('0.1.4', { info: { productVersion: '0.1.3' } }),
    /desktop\/VERSION is canonical/,
  );
  assert.throws(() => appVersionFromSources('0.1.4', { info: {} }), /desktop\/VERSION is canonical/);
  assert.throws(() => appVersionFromSources('0.1.4', undefined), /desktop\/VERSION is canonical/);
});

test('injects the app version while preserving other build arguments', () => {
  assert.deepEqual(versionedWailsBuildArgs(['-tags', 'webkit2_41'], '0.1.3'), [
    '-trimpath',
    '-tags',
    'webkit2_41,with_utls,with_external_windivert',
    '-ldflags',
    '-X github.com/openrung/openrung/connectcore/client.appVersion=0.1.3',
  ]);
});

// A default go build embeds the builder's GOMODCACHE paths — and with them
// the local username — in the binary's debug data. Every packaged build must
// carry -trimpath, injected here so no packaging script can lose it, and a
// caller passing it explicitly must not double the flag.
test('every build carries -trimpath exactly once', () => {
  for (const args of [[], ['-debug'], ['-trimpath'], ['--trimpath'], ['-trimpath', '-tags', 'webkit2_41']]) {
    const built = versionedWailsBuildArgs(args, '0.1.3');
    assert.equal(
      built.filter((argument) => argument === '-trimpath' || argument === '--trimpath').length,
      1,
      `${JSON.stringify(args)} did not carry -trimpath exactly once: ${JSON.stringify(built)}`,
    );
  }
});

// The bundled sing-box engine is only reachable through a with_utls build, so
// a package built without it compiles, installs, and then fails every connect.
// Injecting the tags here means no packaging script or workflow step can lose
// them, and no caller -tags value can displace them.
test('every build carries the sing-box engine build tags', () => {
  assert.deepEqual(singBoxBuildTags, ['with_utls', 'with_external_windivert']);
  for (const args of [[], ['-tags', 'webkit2_41'], ['-tags=webkit2_41'], ['-debug']]) {
    const built = versionedWailsBuildArgs(args, '0.1.3');
    const tags = built[built.indexOf('-tags') + 1].split(',');
    for (const tag of singBoxBuildTags) {
      assert.ok(tags.includes(tag), `${JSON.stringify(args)} lost the ${tag} build tag`);
    }
  }
});

test('merges tags into one comma-joined value Wails accepts', () => {
  // Wails rejects a -tags value that mixes spaces and commas, and honors only
  // the last of repeated flags — so both forms must collapse into one value.
  assert.equal(mergedBuildTags([]), 'with_utls,with_external_windivert');
  assert.equal(mergedBuildTags(['webkit2_41 hidden']), 'webkit2_41,hidden,with_utls,with_external_windivert');
  assert.equal(
    mergedBuildTags(['webkit2_41,hidden', 'extra']),
    'webkit2_41,hidden,extra,with_utls,with_external_windivert',
  );
  // A caller that already passes one of ours must not duplicate it.
  assert.equal(mergedBuildTags(['with_utls']), 'with_utls,with_external_windivert');
  assert.deepEqual(versionedWailsBuildArgs(['-tags', 'a', '-tags=b'], '0.1.3'), [
    '-trimpath',
    '-tags',
    'a,b,with_utls,with_external_windivert',
    '-ldflags',
    '-X github.com/openrung/openrung/connectcore/client.appVersion=0.1.3',
  ]);
});

test('merges separate and equals-form caller ldflags before the app version', () => {
  assert.deepEqual(
    versionedWailsBuildArgs(
      ['-ldflags', '-s -w', '-debug', '-ldflags=-X github.com/openrung/openrung/connectcore/client.appVersion=custom'],
      '0.1.3',
    ),
    [
      '-debug',
      '-trimpath',
      '-tags',
      'with_utls,with_external_windivert',
      '-ldflags',
      '-s -w -X github.com/openrung/openrung/connectcore/client.appVersion=custom -X github.com/openrung/openrung/connectcore/client.appVersion=0.1.3',
    ],
  );
});

test('rejects a non-semantic build version', () => {
  for (const version of [undefined, '', 'dev', 'v0.1.3']) {
    assert.throws(
      () => versionedWailsBuildArgs([], version),
      /must be a semantic X\.Y\.Z version/,
    );
  }
});

test('rejects a caller ldflags or tags option without a value', () => {
  assert.throws(() => versionedWailsBuildArgs(['-ldflags'], '0.1.3'), /-ldflags requires a value/);
  assert.throws(() => versionedWailsBuildArgs(['-tags'], '0.1.3'), /-tags requires a value/);
});
