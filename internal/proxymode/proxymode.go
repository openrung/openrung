// Package proxymode sets and restores the OS system proxy for the end-user
// client's default (zero-privilege) connect mode. The app owns the proxy — it
// snapshots the user's prior settings before pointing them at the local
// sing-box mixed inbound and restores them on disconnect, crash, or next
// launch. This is deliberately NOT sing-box's own set_system_proxy, which does
// not restore a pre-existing proxy (censorship users often chain proxies) and
// is silently ignored outside Windows/macOS/GNOME.
//
// Linux support covers GNOME-family desktops (GSettings) and KDE Plasma
// (kioslaverc); other desktops fall back to a manual proxy address.
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
	// AutoProxy is the service's automatic proxy configuration: its PAC URL
	// and automatic proxy discovery (WPAD). Nil in a snapshot taken before it
	// was captured: that version's Set left both alone, so its Restore must too.
	AutoProxy *AutoProxyState `json:"auto_proxy,omitempty"`
}

// AutoProxyState is a macOS network service's automatic proxy configuration.
// Enabled and URL are the PAC setting; Discovery is automatic proxy discovery,
// which finds a PAC file through the network (WPAD).
type AutoProxyState struct {
	Enabled   bool   `json:"enabled"`
	URL       string `json:"url,omitempty"`
	Discovery bool   `json:"discovery"`
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
	// AutoDetect is "Automatically detect settings" (WPAD). Nil in a snapshot
	// taken before it was captured: that version's Set left it alone, so its
	// Restore must too.
	AutoDetect *bool `json:"auto_detect,omitempty"`
}

// LinuxProxyState is the session desktop's proxy configuration, captured as
// raw values so a Restore brings back a prior manual or auto-config (PAC)
// proxy. Desktop names the session it came from ("gnome" or "kde"); Settings
// maps each captured key to its raw value: "<schema> <key>" to GVariant text
// for org.gnome.system.proxy, and the [Proxy Settings] key to its string for
// kioslaverc. A "kde" capture holds both when GSettings was also driven.
type LinuxProxyState struct {
	Desktop  string            `json:"desktop"`
	Settings map[string]string `json:"settings,omitempty"`
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
	// Linux carries the desktop proxy capture; set only on Linux snapshots.
	Linux *LinuxProxyState `json:"linux,omitempty"`
	// DNS carries a DNSController capture; set only on DNS snapshots, which
	// share the persisted snapshot slot with proxy snapshots. A proxy
	// Restore ignores it and a DNS Restore ignores everything else.
	DNS []ServiceDNSState `json:"dns,omitempty"`
}

// ServiceDNSState is one macOS network service's manually configured DNS
// servers. An empty list means none were configured, so the service used the
// servers its network supplied.
type ServiceDNSState struct {
	Name    string   `json:"name"`
	Servers []string `json:"servers,omitempty"`
}

// DNSController points the OS resolver at a tunnel's DNS address and restores
// it, for TUN mode on platforms where the tunnel itself does not.
type DNSController interface {
	// Supported reports whether resolver control works here.
	Supported() bool
	// Snapshot returns prev extended with the current settings of every
	// service prev does not hold yet. Services prev already holds keep their
	// captured settings, so a re-snapshot after Set never records the
	// tunnel's own server as the user's. Pass a zero Snapshot first.
	Snapshot(prev Snapshot) (Snapshot, error)
	// Set points the resolver of every service snap holds at server and
	// changes nothing else, so every change is covered by snap.
	Set(server string, snap Snapshot) error
	// Restore reverts to a previously captured snapshot.
	Restore(snap Snapshot) error
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
