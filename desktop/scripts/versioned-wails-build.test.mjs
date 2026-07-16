import assert from 'node:assert/strict';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import {
  productVersionFromConfig,
  readProductVersion,
  versionedWailsBuildArgs,
} from './versioned-wails-build.mjs';

test('reads the product version from wails.json', () => {
  const directory = mkdtempSync(join(tmpdir(), 'openrung-version-'));
  const source = join(directory, 'wails.json');
  try {
    writeFileSync(source, JSON.stringify({ info: { productVersion: '4.5.6' } }));
    assert.equal(readProductVersion(source), '4.5.6');
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test('accepts only canonical X.Y.Z product versions', () => {
  assert.equal(productVersionFromConfig({ info: { productVersion: '0.1.3' } }), '0.1.3');

  for (const productVersion of [undefined, 13, '', 'v0.1.3', '0.1', '0.1.3-beta.1', '01.2.3', ' 0.1.3']) {
    assert.throws(
      () => productVersionFromConfig({ info: { productVersion } }),
      /info\.productVersion must be a semantic X\.Y\.Z version/,
    );
  }
});

test('injects the product version while preserving other build arguments', () => {
  assert.deepEqual(versionedWailsBuildArgs(['-tags', 'webkit2_41'], '0.1.3'), [
    '-tags',
    'webkit2_41',
    '-ldflags',
    '-X openrung/internal/client.appVersion=0.1.3',
  ]);
});

test('merges separate and equals-form caller ldflags before the product version', () => {
  assert.deepEqual(
    versionedWailsBuildArgs(
      ['-ldflags', '-s -w', '-debug', '-ldflags=-X openrung/internal/client.appVersion=custom'],
      '0.1.3',
    ),
    [
      '-debug',
      '-ldflags',
      '-s -w -X openrung/internal/client.appVersion=custom -X openrung/internal/client.appVersion=0.1.3',
    ],
  );
});

test('rejects a caller ldflags option without a value', () => {
  assert.throws(() => versionedWailsBuildArgs(['-ldflags'], '0.1.3'), /-ldflags requires a value/);
});
