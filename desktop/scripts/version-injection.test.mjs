import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

import { versionedWailsBuildArgs } from './versioned-wails-build.mjs';

const desktopDirectory = fileURLToPath(new URL('..', import.meta.url));

// Lives apart from versioned-wails-build.test.mjs because it needs a Go
// toolchain: it runs under go-checks, not the frontend workflow.
//
// The Go linker silently ignores -X for a symbol it cannot resolve — the build
// still succeeds and the variable keeps its default. So renaming appVersion,
// moving its package, or dropping the import from the desktop binary would
// leave every release reporting "dev" to broker telemetry while the About
// screen still showed the right number (the frontend reads wails.json
// independently) and every other check stayed green. Link the probe with the
// flags the packaging script really produces and read the value back.
test('the packaging ldflags actually set the Go app version', () => {
  const probeVersion = '9.9.9';
  const args = versionedWailsBuildArgs([], probeVersion);
  const ldflags = args[args.indexOf('-ldflags') + 1];

  const result = spawnSync('go', ['run', '-ldflags', ldflags, './scripts/versionprobe'], {
    cwd: desktopDirectory,
    encoding: 'utf8',
  });

  assert.equal(result.error, undefined, `could not run go: ${result.error?.message}`);
  assert.equal(result.status, 0, `go run failed: ${result.stderr}`);
  assert.equal(
    result.stdout.trim(),
    probeVersion,
    'the injected -X flag did not reach client.AppVersion(); the symbol path in ' +
      'versioned-wails-build.mjs no longer resolves, so releases would report "dev"',
  );
});
