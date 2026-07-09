// Package proxymode sets and restores the OS system proxy for the desktop
// client's default (zero-privilege) connect mode. The GUI owns the proxy — it
// snapshots the user's prior settings before pointing them at the local
// sing-box mixed inbound and restores them on disconnect, crash, or next
// launch. This is deliberately NOT sing-box's own set_system_proxy, which does
// not restore a pre-existing proxy (censorship users often chain proxies) and
// is silently ignored outside Windows/macOS/GNOME.
package proxymode

// ServiceProxyState is one OS network service's HTTP/HTTPS proxy settings,
// captured so they can be restored verbatim.
type ServiceProxyState struct {
	Name          string `json:"name"`
	WebEnabled    bool   `json:"web_enabled"`
	WebHost       string `json:"web_host"`
	WebPort       int    `json:"web_port"`
	SecureEnabled bool   `json:"secure_enabled"`
	SecureHost    string `json:"secure_host"`
	SecurePort    int    `json:"secure_port"`
}

// WindowsProxyState is the user's global WinInet proxy configuration (a single
// per-user setting, not one entry per network service like macOS), captured
// verbatim so a Restore reproduces it exactly — including a chained upstream
// proxy or PAC URL a censorship user may depend on.
type WindowsProxyState struct {
	ProxyEnable   bool   `json:"proxy_enable"`
	ProxyServer   string `json:"proxy_server"`
	ProxyOverride string `json:"proxy_override"`
	AutoConfigURL string `json:"auto_config_url"`
}

// Snapshot is a restorable capture of the OS proxy state. It is persisted to
// disk while connected so a crash can be cleaned up on the next launch. Platform
// tags the snapshot so a restore refuses to apply cross-platform data.
type Snapshot struct {
	Platform string              `json:"platform"`
	Services []ServiceProxyState `json:"services,omitempty"`
	// Windows carries the global WinInet capture; set only on Windows
	// snapshots, nil elsewhere.
	Windows *WindowsProxyState `json:"windows,omitempty"`
}

// Controller sets and restores the OS system proxy on one platform.
type Controller interface {
	// Supported reports whether OS proxy control works here (platform + desktop
	// environment). When false, the caller falls back to advertising the
	// loopback proxy address for manual configuration.
	Supported() bool
	// Snapshot captures the current settings for a later Restore.
	Snapshot() (Snapshot, error)
	// Set points the OS proxy at host:port (the local mixed inbound).
	Set(host string, port int) error
	// Restore reverts to a previously captured snapshot.
	Restore(snap Snapshot) error
}
