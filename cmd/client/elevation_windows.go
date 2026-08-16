package main

import "golang.org/x/sys/windows"

const (
	elevationPrivilege = "Administrator privileges"
	// %s is the invoked program name; Windows has no sudo, so the elevation
	// happens when the terminal itself is started.
	elevationRerunFormat = "rerun `%s connect --tun` from a terminal started with Run as administrator"
)

// elevated reports whether this process runs with an elevated token, which is
// what creating the Wintun adapter and rewriting the routing table requires.
func elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }
