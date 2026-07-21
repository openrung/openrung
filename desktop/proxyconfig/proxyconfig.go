// SPDX-License-Identifier: GPL-3.0-or-later

// Package proxyconfig owns the desktop client's stable local proxy endpoint
// and the opt-in shell integration that exposes it to command-line programs.
package proxyconfig

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"openrung/desktop/persist"
)

const (
	// Host is intentionally fixed to IPv4 loopback. The mixed HTTP/SOCKS
	// inbound has no authentication and must never become a LAN-facing proxy.
	Host = "127.0.0.1"
	// PortEnv is the supported process-level override for the stable port.
	PortEnv = "OPENRUNG_PROXY_PORT"
	// ShellProxyEnv marks an environment activated by OpenRung's generated
	// helper. A child OpenRung process uses it to avoid proxying its own
	// bootstrap requests through the not-yet-listening local endpoint.
	ShellProxyEnv = "OPENRUNG_SHELL_PROXY"
)

// Info is the user-facing local proxy configuration. The shell commands are
// copyable: EnableCommand sources the generated helper and activates it in the
// current POSIX shell; DisableCommand restores that shell's previous values.
type Info struct {
	Host           string
	Port           int
	Endpoint       string
	HelperPath     string
	EnableCommand  string
	DisableCommand string
}

// PortResolution separates a usable process-local endpoint from a non-fatal
// persistence warning. Losing persistence must never prevent access, but the
// UI should not promise restart stability when saving failed.
type PortResolution struct {
	Port               int
	PersistenceWarning error
}

// ResolvePort selects one port for this installation. An explicit environment
// override wins but is not persisted; otherwise the stored port is reused, or
// a kernel-selected loopback port is allocated and saved for future launches.
func ResolvePort(store *persist.Store) (PortResolution, error) {
	return resolvePort(store, os.LookupEnv, freeLoopbackPort)
}

func resolvePort(store *persist.Store, lookupEnv func(string) (string, bool), allocate func() (int, error)) (PortResolution, error) {
	if raw, ok := lookupEnv(PortEnv); ok && strings.TrimSpace(raw) != "" {
		port, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || !validPort(port) {
			return PortResolution{}, fmt.Errorf("%s must be an integer from 1 to 65535 (got %q)", PortEnv, raw)
		}
		return PortResolution{Port: port}, nil
	}
	if store != nil {
		if port, ok := store.LoadProxyPort(); ok {
			return PortResolution{Port: port}, nil
		}
	}
	port, err := allocate()
	if err != nil {
		return PortResolution{}, fmt.Errorf("allocate local proxy port: %w", err)
	}
	if !validPort(port) {
		return PortResolution{}, fmt.Errorf("allocator returned invalid proxy port %d", port)
	}
	resolution := PortResolution{Port: port}
	if store == nil {
		resolution.PersistenceWarning = errors.New("could not persist the local proxy port; endpoint may change next launch: configuration directory is unavailable")
	} else {
		// Persistence is a convenience, not a prerequisite for access. Keep the
		// endpoint process-stable even when a read-only or damaged config
		// directory prevents it from surviving the next launch. The locked
		// operation also returns another process's winner on simultaneous first
		// launch, so every process agrees with the persisted endpoint.
		if persisted, persistErr := store.LoadOrSaveProxyPort(port); persistErr == nil {
			resolution.Port = persisted
		} else {
			resolution.PersistenceWarning = fmt.Errorf("could not persist the local proxy port; endpoint may change next launch: %w", persistErr)
		}
	}
	return resolution, nil
}

// SanitizeInheritedProxyEnvironment removes only proxy values installed by
// OpenRung's generated helper. This must run at the very start of the desktop
// process: launching OpenRung from an activated shell would otherwise send its
// own broker bootstrap through a local proxy that cannot exist until bootstrap
// finishes. Unrelated user-configured upstream proxies are left untouched.
func SanitizeInheritedProxyEnvironment() {
	endpoint, ok := os.LookupEnv(ShellProxyEnv)
	if !ok {
		return
	}
	defer func() {
		_ = os.Unsetenv(ShellProxyEnv)
		for _, variable := range shellVariables(1) {
			_ = os.Unsetenv(savedValueName(variable))
			_ = os.Unsetenv(savedSetName(variable))
			_ = os.Unsetenv(savedExportedName(variable))
		}
	}()

	host, rawPort, err := net.SplitHostPort(strings.TrimSpace(endpoint))
	if err != nil || host != Host {
		return
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || !validPort(port) {
		return
	}

	for _, variable := range shellVariables(port) {
		if variable.name != ShellProxyEnv {
			if current, exists := os.LookupEnv(variable.name); exists && current == variable.value {
				if os.Getenv(savedSetName(variable)) == "1" && os.Getenv(savedExportedName(variable)) == "1" {
					if original, saved := os.LookupEnv(savedValueName(variable)); saved {
						_ = os.Setenv(variable.name, original)
					} else {
						_ = os.Unsetenv(variable.name)
					}
				} else {
					_ = os.Unsetenv(variable.name)
				}
			}
		}
	}
}

// EnsureAvailable performs an early, actionable bind check before relay
// discovery. It deliberately does not choose another port: silently rotating a
// stable endpoint would break browser and shell configuration. As before,
// sing-box's later bind retains a small bind-and-close race window.
func EnsureAvailable(port int) error {
	if !validPort(port) {
		return fmt.Errorf("proxy port %d is outside 1..65535", port)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(Host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("local proxy port %d is unavailable; set %s to another unused port: %w", port, PortEnv, err)
	}
	return listener.Close()
}

// WriteShellHelper writes the sourceable helper for port and returns all
// display/copy values derived from the same endpoint.
func WriteShellHelper(store *persist.Store, port int) (Info, error) {
	info, err := EndpointInfo(port)
	if err != nil {
		return Info{}, err
	}
	if store == nil {
		return Info{}, errors.New("proxy configuration directory is unavailable")
	}
	script, err := shellScript(port)
	if err != nil {
		return Info{}, err
	}
	path, err := store.SaveProxyEnvScript(port, []byte(script))
	if err != nil {
		return Info{}, fmt.Errorf("write proxy shell helper: %w", err)
	}
	info.HelperPath = path
	info.EnableCommand = ". " + shellQuote(path) + " && openrung_proxy_on"
	info.DisableCommand = "openrung_proxy_off"
	return info, nil
}

// EndpointInfo returns display metadata without requiring shell-helper I/O.
func EndpointInfo(port int) (Info, error) {
	if !validPort(port) {
		return Info{}, fmt.Errorf("proxy port %d is outside 1..65535", port)
	}
	return Info{
		Host:     Host,
		Port:     port,
		Endpoint: net.JoinHostPort(Host, strconv.Itoa(port)),
	}, nil
}

func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(Host, "0"))
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func validPort(port int) bool {
	return port >= 1 && port <= 65535
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

type shellVariable struct {
	name  string
	saved string
	value string
}

func shellVariables(port int) []shellVariable {
	httpURL := "http://" + net.JoinHostPort(Host, strconv.Itoa(port))
	socksURL := "socks5h://" + net.JoinHostPort(Host, strconv.Itoa(port))
	return []shellVariable{
		{name: "http_proxy", saved: "HTTP_PROXY_LOWER", value: httpURL},
		{name: "https_proxy", saved: "HTTPS_PROXY_LOWER", value: httpURL},
		{name: "HTTP_PROXY", saved: "HTTP_PROXY_UPPER", value: httpURL},
		{name: "HTTPS_PROXY", saved: "HTTPS_PROXY_UPPER", value: httpURL},
		{name: "all_proxy", saved: "ALL_PROXY_LOWER", value: socksURL},
		{name: "ALL_PROXY", saved: "ALL_PROXY_UPPER", value: socksURL},
		{name: ShellProxyEnv, saved: "SHELL_PROXY_MARKER", value: net.JoinHostPort(Host, strconv.Itoa(port))},
	}
}

func savedValueName(variable shellVariable) string {
	return "_OPENRUNG_SAVED_" + variable.saved
}

func savedSetName(variable shellVariable) string {
	return savedValueName(variable) + "_SET"
}

func savedExportedName(variable shellVariable) string {
	return savedValueName(variable) + "_EXPORTED"
}

func shellScript(port int) (string, error) {
	if !validPort(port) {
		return "", fmt.Errorf("proxy port %d is outside 1..65535", port)
	}
	variables := shellVariables(port)

	var out strings.Builder
	out.WriteString("# Generated by OpenRung. Source this file; it does not enable the proxy by itself.\n")
	out.WriteString("# Run openrung_proxy_off after disconnect, failure, quit, or crash.\n\n")
	out.WriteString("openrung_proxy_on() {\n")
	out.WriteString("  if [ \"${_OPENRUNG_PROXY_ACTIVE-}\" != \"1\" ]; then\n")
	for _, variable := range variables {
		savedValue := savedValueName(variable)
		savedSet := savedSetName(variable)
		savedExported := savedExportedName(variable)
		fmt.Fprintf(&out, "    unset %s %s %s\n", savedValue, savedSet, savedExported)
		fmt.Fprintf(&out, "    if [ \"${%s+x}\" = \"x\" ]; then\n", variable.name)
		fmt.Fprintf(&out, "      %s=1\n", savedSet)
		fmt.Fprintf(&out, "      %s=\"${%s}\"\n", savedValue, variable.name)
		fmt.Fprintf(&out, "      if command env | command grep '^%s=' >/dev/null 2>&1; then\n", variable.name)
		fmt.Fprintf(&out, "        %s=1\n", savedExported)
		fmt.Fprintf(&out, "        export %s\n", savedValue)
		out.WriteString("      else\n")
		fmt.Fprintf(&out, "        %s=0\n", savedExported)
		out.WriteString("      fi\n")
		out.WriteString("    else\n")
		fmt.Fprintf(&out, "      %s=0\n", savedSet)
		fmt.Fprintf(&out, "      %s=0\n", savedExported)
		out.WriteString("    fi\n")
		fmt.Fprintf(&out, "    export %s %s\n", savedSet, savedExported)
	}
	out.WriteString("    _OPENRUNG_PROXY_ACTIVE=1\n")
	out.WriteString("  fi\n")
	for _, variable := range variables {
		fmt.Fprintf(&out, "  %s=%s\n", variable.name, shellQuote(variable.value))
		fmt.Fprintf(&out, "  export %s\n", variable.name)
	}
	out.WriteString("}\n\n")

	out.WriteString("openrung_proxy_off() {\n")
	out.WriteString("  if [ \"${_OPENRUNG_PROXY_ACTIVE-}\" != \"1\" ]; then\n")
	out.WriteString("    return 0\n")
	out.WriteString("  fi\n")
	for _, variable := range variables {
		savedValue := savedValueName(variable)
		savedSet := savedSetName(variable)
		savedExported := savedExportedName(variable)
		fmt.Fprintf(&out, "  if [ \"${%s-0}\" = \"1\" ]; then\n", savedSet)
		fmt.Fprintf(&out, "    unset %s\n", variable.name)
		fmt.Fprintf(&out, "    %s=\"${%s-}\"\n", variable.name, savedValue)
		fmt.Fprintf(&out, "    if [ \"${%s-0}\" = \"1\" ]; then\n", savedExported)
		fmt.Fprintf(&out, "      export %s\n", variable.name)
		out.WriteString("    fi\n")
		out.WriteString("  else\n")
		fmt.Fprintf(&out, "    unset %s\n", variable.name)
		out.WriteString("  fi\n")
		fmt.Fprintf(&out, "  unset %s %s %s\n", savedValue, savedSet, savedExported)
	}
	out.WriteString("  unset _OPENRUNG_PROXY_ACTIVE\n")
	out.WriteString("}\n")
	return out.String(), nil
}
