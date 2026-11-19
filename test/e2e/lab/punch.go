// SPDX-License-Identifier: GPL-3.0-or-later

//go:build linux

package lab

import "strconv"

// HubReflectorPort is the UDP port of the relay hub's reflector, which tells
// each side of a NAT punch the public address and port its router gave it.
const HubReflectorPort = 19302

// HubReflectorAddrs binds the reflector on both of the hub's public
// addresses. With two, the hub can tell a router that keeps one public port
// per socket (punchable) from one that picks a new port per destination.
var HubReflectorAddrs = HubAddr + ":" + strconv.Itoa(HubReflectorPort) + "," +
	HubAltAddr + ":" + strconv.Itoa(HubReflectorPort)
