import { spawnSync } from 'node:child_process';
import { readFileSync, realpathSync } from 'node:fs';
import { fileURLToPath, pathToFileURL } from 'node:url';

const versionPath = fileURLToPath(new URL('../VERSION', import.meta.url));
const configPath = fileURLToPath(new URL('../wails.json', import.meta.url));
const semanticVersion = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/;
const appVersionVariable = 'openrung/internal/client.appVersion';

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

export function versionedWailsBuildArgs(args, version) {
  if (typeof version !== 'string' || !semanticVersion.test(version)) {
    throw new Error(`build version ${JSON.stringify(version)} must be a semantic X.Y.Z version`);
  }

  const passthrough = [];
  const callerLdflags = [];
  for (let index = 0; index < args.length; index += 1) {
    const argument = args[index];
    if (argument === '-ldflags' || argument === '--ldflags') {
      if (index + 1 >= args.length) {
        throw new Error(`${argument} requires a value`);
      }
      callerLdflags.push(args[index + 1]);
      index += 1;
      continue;
    }

    const equalsForm = ['-ldflags=', '--ldflags='].find((prefix) => argument.startsWith(prefix));
    if (equalsForm !== undefined) {
      callerLdflags.push(argument.slice(equalsForm.length));
      continue;
    }

    passthrough.push(argument);
  }

  // Keep caller flags, but append the source-of-truth assignment last so a
  // caller cannot accidentally replace the version from desktop/VERSION.
  const versionLdflag = `-X ${appVersionVariable}=${version}`;
  const ldflags = [...callerLdflags.filter((value) => value.trim() !== ''), versionLdflag].join(' ');
  return [...passthrough, '-ldflags', ldflags];
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
