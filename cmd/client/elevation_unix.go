//go:build !windows

package main

import "os"

const (
	elevationPrivilege = "root privileges"
	// %s is the invoked program name.
	elevationRerunFormat = "rerun as `sudo %s connect --tun`"
)

// elevated reports whether this process may create a TUN device and install
// routes. The check is deliberately the effective uid rather than an
// inspection of Linux capabilities: sudo is the documented and portable way to
// run the client in TUN mode, and refusing a CAP_NET_ADMIN-only process costs
// a rerun, while wrongly admitting one would surface as an opaque sing-box
// failure after the ladder had already dialed relays.
func elevated() bool { return os.Geteuid() == 0 }
