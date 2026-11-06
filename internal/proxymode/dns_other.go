// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !darwin

package proxymode

import "errors"

// NewDNS returns an unsupported controller off macOS: sing-tun points the
// system resolver at the tunnel itself on Windows and on Linux with
// systemd-resolved.
func NewDNS() DNSController {
	return unsupportedDNSController{}
}

type unsupportedDNSController struct{}

func (unsupportedDNSController) Supported() bool { return false }

func (unsupportedDNSController) Snapshot() (Snapshot, error) { return Snapshot{}, nil }

func (unsupportedDNSController) Set(string) error {
	return errors.New("system DNS control is not implemented on this platform")
}

func (unsupportedDNSController) Restore(Snapshot) error { return nil }
