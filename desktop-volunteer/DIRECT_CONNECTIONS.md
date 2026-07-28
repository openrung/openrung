# Direct connections

OpenRung Volunteer prefers a direct TCP connection because it removes an
unnecessary relay hop. Local setup can make TCP 443 usable by this application,
but it cannot prove that the computer is reachable from the Internet. In
**Automatic** mode, OpenRung advertises a direct endpoint only after RelayHub's
nonce callback positively confirms it.

## Port selection and fallback

In **Automatic** mode, the desktop app checks candidates in this order:

1. TCP 443.
2. The configured alternate port (8443 on existing and default installations).
3. RelayHub, after every viable direct candidate fails.

The candidate list is deduplicated when the alternate is already 443. A failed
local bind, an occupied port, a failed external callback, or an unavailable
probe API never counts as direct reachability. While using RelayHub, periodic
checks repeat the same order and promote the session when a candidate is
positively reachable.

**Direct only** is intentionally different: it uses exactly the configured
port, never uses RelayHub, and does not perform RelayHub's nonce reachability
check. Choose it only when this computer already has a publicly reachable
address and inbound TCP is configured. The one-time TCP 443 setup does not
change that setting or make Direct only try extra ports.

OpenRung does not automatically try ports 22, 80, or 3389.

## What the setup button does

On a supported and eligible install, Settings contains an explicit **Enable
direct connections** action. Merely opening OpenRung, inspecting setup status,
or starting volunteering does not request elevation. The whole GUI is never
relaunched as Administrator, root, or with `sudo`; manually elevated GUI
startup is refused.

If authorization is declined, setup fails, or the platform cannot support it,
volunteering is still available in Automatic mode: OpenRung tries any distinct
configured alternate port and then RelayHub. Setup is idempotent and can be
retried after stopping the relay. When OpenRung owns reversible setup, Settings
also offers **Remove local setup**.

This setup affects only the local computer. It does not:

- create router port forwarding or a NAT mapping;
- obtain a public address or bypass carrier-grade NAT;
- open an AWS, Tencent Cloud, or other cloud firewall/security group;
- disable or broadly open a host firewall; or
- grant Xray permission to bind a low port.

OpenRung's ConnectionObserver owns the public TCP listener and forwards traffic
to Xray on loopback. Xray therefore does not receive low-port capability.

## Windows

Windows does not require elevation merely to bind TCP 443. The explicit setup
action uses UAC only to manage one Windows Defender Firewall inbound rule:

- internal name: `OpenRungVolunteer-Direct-TCP-443-v1`
- display name: `OpenRung Volunteer — Direct TCP 443`
- group: `OpenRung Volunteer`
- direction/action: inbound/allow
- protocol/local port: TCP/443
- program: the exact current `OpenRungVolunteer.exe`
- profiles: all; enabled in the persistent policy store

No untrusted path is interpolated into PowerShell source. An unprivileged
system PowerShell launcher receives a minimal environment and asks UAC to
elevate the fixed, system PowerShell executable—not the portable Wails GUI.
The canonical application path travels only as base64 data to the fixed
firewall operation. The elevated stage disables module autoload, imports
NetSecurity from the fixed system manifest, and uses module-qualified cmdlets.
A read-only status check verifies the complete rule. If the executable moves
or an update changes its path, setup is reported as stale; the next explicit
Enable action replaces the fixed-name rule instead of adding a duplicate.

Use **Remove local setup** after stopping the relay. If the application is no
longer available, an administrator can remove only OpenRung's fixed rule from
an elevated PowerShell:

```powershell
Remove-NetFirewallRule -Name 'OpenRungVolunteer-Direct-TCP-443-v1'
```

The current Windows artifact is a ZIP, not an installer, so deleting the
directory does not run cleanup automatically. A future installer must invoke
this targeted removal during uninstall; it must not delete unrelated firewall
rules.

## Linux

Do not run the GUI with `sudo`. When the kernel still treats 443 as a privileged
port, the explicit setup action is available only when the GUI binary and every
directory in its installed path are root-owned and not group- or
world-writable. This prevents a portable-path replacement while the polkit
prompt is open. For an eligible installation, `pkexec` starts the same binary
in one fixed, non-GUI helper mode. The authorized helper rechecks that no
unexpected capability appeared during the prompt, invokes the known `setcap`
binary with separate arguments equivalent to the command below, and exits:

```text
setcap cap_net_bind_service=ep '/canonical/path/to/OpenRungVolunteer'
```

Only `CAP_NET_BIND_SERVICE` is granted, and only to the canonical OpenRung GUI
binary. `CAP_NET_ADMIN`, other capabilities, and the bundled Xray binary are
not granted. Status also recognizes systems whose
`net.ipv4.ip_unprivileged_port_start` already permits binding 443 without a
file capability. If the host still requires a capability and OpenRung is
running from a user-writable extracted tarball, setup is reported unavailable
rather than elevating against that path; Automatic continues with the
alternate port and RelayHub.

An administrator who wants TCP 443 can install both packaged binaries in a
stable root-owned directory, then launch that installed GUI:

```sh
sudo install -d -o root -g root -m 0755 /opt/openrung-volunteer
sudo install -o root -g root -m 0755 OpenRungVolunteer /opt/openrung-volunteer/OpenRungVolunteer
sudo install -o root -g root -m 0755 xray /opt/openrung-volunteer/xray
/opt/openrung-volunteer/OpenRungVolunteer
```

This installation step does not grant a capability; the volunteer still
chooses **Enable direct connections** in the unprivileged GUI.

File capabilities apply when the executable starts. After a successful grant,
quit and reopen OpenRung before expecting TCP 443 to bind. If the exact
capability is still ineffective on that new launch, OpenRung reports a possible
`nosuid`, no-new-privileges, filesystem, or security-policy problem instead of
asking for endless restarts. Replacing the binary during an update usually
removes its extended-attribute capability; the app detects that and offers
setup again. A filesystem without capability support is reported as
unavailable rather than treated as external unreachability.

Use **Remove local setup** after stopping the relay when the app reports the
exact OpenRung-managed capability. Removing the xattr does not revoke a
capability already loaded by this process, so quit and reopen OpenRung after
removal; the app prevents volunteering from restarting until then. For manual
cleanup, first inspect the file:

```sh
getcap '/absolute/path/to/OpenRungVolunteer'
```

Run the following only when the output contains exactly
`cap_net_bind_service=ep`; `setcap -r` removes every file capability, not one
selected capability:

```sh
sudo setcap -r '/absolute/path/to/OpenRungVolunteer'
```

If `getcap` reports any additional or different capability, stop and have an
administrator review it rather than deleting unrelated security metadata.

The app does not modify Linux firewall software because distributions and
local policies differ. Determine which firewall is active before making a
change. For example, inspect UFW with `sudo ufw status` or firewalld with
`firewall-cmd --state` and `firewall-cmd --list-ports`. If you explicitly
choose to expose this host, the matching administrator commands are:

```sh
sudo ufw allow 443/tcp
sudo ufw delete allow 443/tcp
```

or, for firewalld, first identify the active zone attached to the intended
network interface:

```sh
firewall-cmd --get-active-zones
sudo firewall-cmd --zone='<active-zone>' --permanent --add-port=443/tcp
sudo firewall-cmd --reload
sudo firewall-cmd --zone='<active-zone>' --permanent --remove-port=443/tcp
sudo firewall-cmd --reload
```

Use only the firewall actually installed and governed by the machine's local
policy. These manual rules open TCP 443 at the host firewall; they are not
router forwarding and still do not guarantee external reachability.

## macOS

The GUI is never run with `sudo`, and OpenRung does not use a setuid binary,
password piping, or an unauthenticated root service. A safe macOS design
requires a minimal privileged listener installed with Apple's ServiceManagement
APIs (`SMAppService` where supported), authenticated local IPC, fixed TCP 443
semantics, lifecycle cleanup, and a stable signing identity shared with the
application.

The current macOS package is ad-hoc signed. The repository does not contain the
Developer ID identity, notarization credentials, signed helper, or installer
lifecycle needed for macOS to authenticate that privileged component.
Consequently, this artifact reports local TCP 443 setup as unavailable and
continues with the alternate port and RelayHub in Automatic mode. macOS TCP
443 setup must not be described as production-ready until the signed packaged
helper's install, update, IPC authentication, listener forwarding, and removal
paths have all been validated on supported macOS versions.

## Router, NAT, IPv6, and cloud requirements

After local setup:

- On IPv4 behind a router, forward external **TCP 443** to TCP 443 on this
  computer and keep its LAN address stable. No UDP rule is part of this feature.
- Carrier-grade NAT generally cannot be fixed with a home-router rule; use
  RelayHub or ask the ISP for a reachable address.
- With public IPv6, allow inbound TCP 443 to this computer in the router and
  host firewall. Do not expose unrelated ports.
- On a cloud VM, explicitly allow inbound TCP 443 in the applicable provider
  firewall/security group and in the guest firewall. This app does not change
  Tencent Cloud, AWS, or other infrastructure.

In Automatic mode, the external nonce callback remains authoritative after
every local or network change. Direct only does not perform that check.

## Reading outcomes

Automatic mode's console distinguishes the following:

- **Permission denied**: the local process lacks permission to bind the port.
- **Already in use**: another local process owns the port.
- **Externally unreachable/firewalled**: local bind succeeded, but the nonce
  callback did not arrive.
- **Probe API unavailable**: positive reachability could not be established,
  so the port is not advertised as direct.
- **Reachable**: the callback positively confirmed the selected host and port.

An external callback failure is not reported as an operating-system permission
failure. When the active result is **Via RelayHub**, the UI also shows whether
local TCP 443 setup is missing or whether no candidate was positively
confirmed. The console retains the specific local-bind, external, or probe-API
outcome.
