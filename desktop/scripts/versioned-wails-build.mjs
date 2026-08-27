import { spawnSync } from 'node:child_process';
import { readFileSync, realpathSync } from 'node:fs';
import { fileURLToPath, pathToFileURL } from 'node:url';

const versionPath = fileURLToPath(new URL('../VERSION', import.meta.url));
const configPath = fileURLToPath(new URL('../wails.json', import.meta.url));
const semanticVersion = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
const appVersionVariable = 'github.com/openrung/openrung/connectcore/client.appVersion';

// The bundled sing-box engine's build tags (internal/singboxruntime). Every
// desktop package must carry them, for the same reasons every cmd/client build
// does (Makefile, .github/workflows/client-release.yml):
//
//   with_utls                the uTLS/Reality client — without it the linked
//                            engine cannot dial any relay's Reality endpoint,
//                            so the app builds and then fails every connect
//   with_external_windivert  keeps sing-box's embedded WinDivert64.sys driver
//                            (LGPLv3; serves Windows bridge/tlsspoof backends
//                            this app never uses) out of the Windows binary
//                            and the release packages
//
// They are injected here rather than spelled out in each packaging script so
// no build path can lose them; the release workflow's version smoke test is
// the second line of defence.
export const singBoxBuildTags = ['with_utls', 'with_external_windivert'];

// desktop/VERSION is the canonical version source. wails.json keeps an
// info.productVersion copy because Wails stamps it into the native package
// metadata (Info.plist, the Windows exe resource); refuse to build when the
// copy has drifted so the two can never disagree in a shipped artifact.
export function appVersionFromSources(
  versionFileContents,
  config,
  versionSource = 'desktop/VERSION',
  configSource = 'desktop/wails.json',
) {
  const version = String(versionFileContents ?? '').trim();
  if (!semanticVersion.test(version)) {
    throw new Error(`${versionSource} must contain a semantic X.Y.Z version`);
  }
  const copy = config?.info?.productVersion;
  if (copy !== version) {
    throw new Error(
      `${configSource} info.productVersion is ${JSON.stringify(copy)} but ${versionSource} is ${version}; ` +
        `${versionSource} is canonical — update info.productVersion to match`,
    );
  }
  return version;
}

export function readAppVersion(versionSource = versionPath, configSource = configPath) {
  let versionFileContents;
  try {
    versionFileContents = readFileSync(versionSource, 'utf8');
  } catch (error) {
    throw new Error(`cannot read ${versionSource}: ${error.message}`, { cause: error });
  }
  let config;
  try {
    config = JSON.parse(readFileSync(configSource, 'utf8'));
  } catch (error) {
    throw new Error(`cannot read ${configSource}: ${error.message}`, { cause: error });
  }
  return appVersionFromSources(versionFileContents, config, versionSource, configSource);
}

// Wails accepts comma- OR space-separated tags but rejects a value mixing
// both, and its -tags is a single flag (a second occurrence would silently win
// over the first). So caller tags are split on either separator, merged with
// the required sing-box tags, and re-emitted as one comma-joined value.
export function mergedBuildTags(callerTags) {
  const tags = [];
  for (const value of [...callerTags, ...singBoxBuildTags]) {
    for (const tag of String(value).split(/[\s,]+/)) {
      if (tag !== '' && !tags.includes(tag)) {
        tags.push(tag);
      }
    }
  }
  return tags.join(',');
}

export function versionedWailsBuildArgs(args, version) {
  if (typeof version !== 'string' || !semanticVersion.test(version)) {
    throw new Error(`build version ${JSON.stringify(version)} must be a semantic X.Y.Z version`);
  }

  const passthrough = [];
  const callerLdflags = [];
  const callerTags = [];
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    const collected = [
      ['-ldflags', callerLdflags],
      ['-tags', callerTags],
    ].find(([name]) => argument === name || argument === `-${name}`);
    if (collected !== undefined) {
      if (index + 1 >= args.length) {
        throw new Error(`${argument} requires a value`);
      }
      collected[1].push(args[index + 1]);
      index += 1;
      continue;
    }

    const equalsForm = [
      ['-ldflags=', callerLdflags],
      ['--ldflags=', callerLdflags],
      ['-tags=', callerTags],
      ['--tags=', callerTags],
    ].find(([prefix]) => argument.startsWith(prefix));
    if (equalsForm !== undefined) {
      equalsForm[1].push(argument.slice(equalsForm[0].length));
      continue;
    }

    // -trimpath is injected below; swallow a caller's copy so the flag is
    // not passed twice.
    if (argument === '-trimpath' || argument === '--trimpath') {
      continue;
    }

    passthrough.push(argument);
  }

  // Keep caller flags, but append the source-of-truth assignment last so a
  // caller cannot accidentally replace the version from desktop/VERSION.
  const versionLdflag = `-X ${appVersionVariable}=${version}`;
  const ldflags = [...callerLdflags.filter((value) => value.trim() !== ''), versionLdflag].join(' ');
  // -trimpath on every build (Wails forwards it to go build): a default build
  // embeds the builder's GOMODCACHE paths (/Users/<name>/go/pkg/mod/...) in
  // the binary's debug data, leaking the local username into anything
  // distributed. Injected here, like the tags, so no build path can lose it.
  return [...passthrough, '-trimpath', '-tags', mergedBuildTags(callerTags), '-ldflags', ldflags];
}

function displayArgument(argument) {
  return /\s/.test(argument) ? JSON.stringify(argument) : argument;
}

function main() {
  const version = readAppVersion();
  const args = versionedWailsBuildArgs(process.argv.slice(2), version);
  console.log(`==> wails build ${args.map(displayArgument).join(' ')}`);

  const result = spawnSync('wails', ['build', ...args], { stdio: 'inherit' });
  if (result.error) {
    throw result.error;
  }
  if (result.signal !== null) {
    throw new Error(`wails build terminated by ${result.signal}`);
  }
  process.exitCode = result.status ?? 1;
}

// Realpath both sides: Node resolves symlinks for the ESM entry point but not
// for argv[1], so through a symlinked checkout this would silently skip main()
// and exit 0 — leaving the packaging scripts to ship whatever stale binary was
// already in build/bin.
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
