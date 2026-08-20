import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import {
  appVersionFromSources,
  readAppVersion,
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
    '-tags',
    'webkit2_41',
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

test('rejects a caller ldflags option without a value', () => {
  assert.throws(() => versionedWailsBuildArgs(['-ldflags'], '0.1.3'), /-ldflags requires a value/);
});
