// Verifies a freshly built desktop binary against the three things a package
// can silently lose, and prints the bundled engine's version line for the
// packaging scripts' GPL notices.
//
// 1. The linked sing-box version is the one this module resolves. A package
//    built from a stale module cache would ship an engine the config builder's
//    goldens were never validated against.
// 2. The build carries `with_utls`. Without it the app compiles and installs
//    normally and then fails every single connect, because every OpenRung
//    relay endpoint is Reality — the failure mode that makes this check worth
//    running on a GUI binary at all.
// 3. A Windows build carries no WinDivert64.sys driver bytes. sing-box embeds
//    that LGPLv3 driver for Windows backends this app never uses unless built
//    with `with_external_windivert`, and shipping it would add license-text
//    obligations THIRD_PARTY_NOTICES.md deliberately does not carry (§8).
//
// It also pins the app version the packaging linker stamped in
// (connectcore/client.appVersion), which is what the app reports to broker
// telemetry: the linker ignores -X for an unresolved symbol, so without this a
// rename would leave releases reporting "dev".
//
// Usage: node scripts/verify-bundled-engine.mjs <path to the built binary>
import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, realpathSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { readAppVersion } from './versioned-wails-build.mjs';

const desktopDirectory = fileURLToPath(new URL('..', import.meta.url));
const singBoxModule = 'github.com/sagernet/sing-box';
const winDivertAsset = 'common/windivert/assets/WinDivert64.sys';
// Enough of the driver to be unmistakable, and cheap to search for.
const driverProbeBytes = 4096;

function goOutput(args) {
  const result = spawnSync('go', args, { cwd: desktopDirectory, encoding: 'utf8' });
  if (result.error) {
    throw new Error(`could not run go: ${result.error.message}`);
  }
  if (result.status !== 0) {
    throw new Error(`go ${args.join(' ')} failed: ${result.stderr.trim()}`);
  }
  return result.stdout.trim();
}

export function verifyVersionOutput(output, appVersion, singBoxVersion) {
  const lines = String(output).trim().split(/\r?\n/);
  const wantApp = `OpenRung/${appVersion}`;
  const wantEngine = `sing-box/${singBoxVersion} (bundled, with_utls)`;
  if (lines[0] !== wantApp) {
    throw new Error(
      `the built binary reports ${JSON.stringify(lines[0])} as its version, want ${JSON.stringify(wantApp)}: ` +
        'the packaging -X app-version injection did not reach the binary ' +
        '(see scripts/version-injection.test.mjs)',
    );
  }
  if (lines[1] !== wantEngine) {
    throw new Error(
      `the built binary reports ${JSON.stringify(lines[1])} as its engine, want ${JSON.stringify(wantEngine)}: ` +
        'the build lost a sing-box build tag or linked a different engine version — ' +
        'an app without with_utls cannot dial any relay (see scripts/versioned-wails-build.mjs)',
    );
  }
  return lines[1];
}

// Compared by content rather than by build tag so a lost tag cannot pass:
// the tag's whole effect is whether these bytes are in the binary.
export function embedsDriver(binaryContents, driverContents) {
  return binaryContents.includes(driverContents.subarray(0, driverProbeBytes));
}

function assertNoWinDivertDriver(binary, moduleVersion) {
  const asset = join(goOutput(['env', 'GOMODCACHE']), `${singBoxModule}@${moduleVersion}`, winDivertAsset);
  if (!existsSync(asset)) {
    // Fail closed: without the reference bytes this check proves nothing, and
    // the module was just compiled, so it is in the cache.
    throw new Error(`cannot read the reference driver at ${asset}; refusing to certify the package without it`);
  }
  if (embedsDriver(readFileSync(binary), readFileSync(asset))) {
    throw new Error(
      `${binary} embeds WinDivert64.sys: the with_external_windivert build tag was lost, and this package ` +
        'would ship an LGPLv3 driver whose license text the notices do not carry ' +
        '(THIRD_PARTY_NOTICES.md section 8)',
    );
  }
}

function main() {
  const binary = process.argv[2];
  if (!binary) {
    throw new Error('usage: node scripts/verify-bundled-engine.mjs <path to the built binary>');
  }
  // The Windows binary is built for the GUI subsystem and has no console of
  // its own; it still writes to the pipe spawnSync hands it.
  const result = spawnSync(binary, ['version'], { encoding: 'utf8' });
  if (result.error) {
    throw new Error(`could not run ${binary}: ${result.error.message}`);
  }
  if (result.status !== 0) {
    throw new Error(`${binary} version exited ${result.status}: ${result.stderr.trim()}`);
  }

  const moduleVersion = goOutput(['list', '-m', '-f', '{{.Version}}', singBoxModule]);
  const engineVersion = moduleVersion.replace(/^v/, '');
  verifyVersionOutput(result.stdout, readAppVersion(), engineVersion);
  if (binary.toLowerCase().endsWith('.exe')) {
    assertNoWinDivertDriver(binary, moduleVersion);
  }
  // stdout carries nothing but the engine's name and version, so a packaging
  // script can capture it as the string its GPL notices embed. Failures go to
  // stderr with a nonzero exit, which is what stops the packaging script.
  console.log(`sing-box ${engineVersion}`);
}

// Realpath as in versioned-wails-build.mjs: through a symlinked checkout a
// plain argv[1] comparison would skip main() and exit 0, waving through the
// build it was meant to reject.
function isEntryPoint() {
  const entry = process.argv[1];
  if (!entry) {
    return false;
  }
  try {
    return import.meta.url === pathToFileURL(realpathSync(entry)).href;
  } catch {
    return false;
  }
}

if (isEntryPoint()) {
  try {
    main();
  } catch (error) {
    console.error(`error: ${error.message}`);
    process.exitCode = 1;
  }
}
