# Volunteer Desktop packaging and direct-setup release gates

The native packaging scripts remain deliberately simple:

- Windows: a ZIP containing `OpenRungVolunteer.exe` and `xray.exe`
- Linux: a tarball containing `OpenRungVolunteer` and `xray`
- macOS: an ad-hoc-signed `.app` containing Xray in `Contents/Resources`

They also include `DIRECT_CONNECTIONS.md` in the artifact. None is currently
an installer, and packaging must not mutate the build machine's firewall,
capabilities, privileged helpers, router, or cloud infrastructure.

## Platform behavior

### Windows

The application binary contains the explicit UAC setup/removal flow for the
fixed, executable-scoped Windows Defender Firewall rule. Its unprivileged,
minimal-environment launcher elevates the system PowerShell executable for the
fixed operation; it never elevates the portable Wails GUI. The ZIP does not add
the rule during extraction and cannot remove it when a user deletes the
directory. Moving the executable invalidates the program path; the app must
show the mismatch and replace the fixed-name rule only after another explicit
Enable action.

If packaging later changes to MSI, MSIX, or NSIS, uninstall must remove only
`OpenRungVolunteer-Direct-TCP-443-v1`. Do not introduce a broad program,
port-range, or allow-all rule.

### Linux

Do not preserve or pre-apply a file capability in the tarball. Installation
paths and filesystems are only known on the volunteer's machine, and local
authorization belongs there. The explicit `pkexec` flow grants only
`cap_net_bind_service=ep` to a canonical GUI binary whose file and ancestor
directories are root-owned and not group/world-writable. When the kernel still
treats 443 as privileged, a user-writable portable extraction is intentionally
ineligible for elevation and retains Automatic alternate-port/RelayHub
fallback. A replaced binary must be detected and set up again. Xray must remain
unprivileged. Removal must require an app restart before volunteering resumes,
because the old process retains its already-loaded capability.

A future Linux installer may establish the documented root-owned application
directory, but it must not pre-grant the capability: authorization remains
behind the in-app user action. Update and uninstall paths must remove or
re-detect only the exact `cap_net_bind_service=ep` state.

No packaging or uninstall script should guess the host's firewall manager.

### macOS

Ad-hoc signing is insufficient for the authenticated privileged-helper design.
The current script therefore packages no helper, launch daemon, or elevated
fallback. It must continue to report TCP 443 local setup as unavailable.

Completing macOS support requires, at minimum:

1. a minimal listener/loopback-forwarding helper with fixed TCP 443 input;
2. authenticated, narrowly typed local IPC (not arbitrary commands or ports);
3. `SMAppService`/ServiceManagement registration and removal;
4. stable Developer ID signing for the app and helper, correct bundle metadata
   and designated requirements;
5. hardened-runtime/notarized packaging and update compatibility; and
6. on-device validation of install, bind, forwarding, upgrade, failure, and
   cleanup paths.

Do not substitute setuid, `sudo`, password piping, or a root GUI.

## Release checklist

The normal CI build proves that each native artifact compiles and that its
bundled Xray checksum matches the pin. It does **not** exercise a real UAC
desktop, polkit authorization agent, firewall policy store, file-capability
filesystem, router, cloud firewall, Developer ID identity, or notarization
service.

Before describing a platform's TCP 443 setup as production-ready, validate the
actual packaged artifact on that platform:

- setup is requested only from the explicit button;
- declining authorization leaves volunteering and Automatic fallback usable;
- repeated Enable is idempotent;
- status catches a moved/replaced application;
- removal touches only exact, recognized OpenRung-managed state and leaves
  unexpected Linux file capabilities for administrator review;
- TCP 443 is owned by ConnectionObserver and Xray remains loopback-only;
- Automatic external reachability is never inferred from local setup;
- Automatic tries 443, then the persisted alternate, then RelayHub;
- a tunnel session periodically promotes after a positive direct callback; and
- artifact install/update/removal behavior matches `DIRECT_CONNECTIONS.md`.

Until those packaged elevation paths have been exercised, report them as
implemented and compile/test checked, not production-validated. The current
macOS artifact is specifically blocked as described above.
