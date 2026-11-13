//go:build windows

// SPDX-License-Identifier: GPL-3.0-or-later

package proxymode

import (
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// New returns the Windows proxy controller. It drives the current user's WinInet
// proxy settings for the default (LAN) connection through the per-connection
// options of InternetQueryOption and InternetSetOption, then signals WinInet so
// already running apps (Edge, Chrome, and anything else on WinInet) pick up the
// change live rather than only on restart.
func New() Controller {
	return &windowsController{backend: wininetBackend{}}
}

var (
	wininet                  = windows.NewLazySystemDLL("wininet.dll")
	procInternetSetOptionW   = wininet.NewProc("InternetSetOptionW")
	procInternetQueryOptionW = wininet.NewProc("InternetQueryOptionW")
	procGlobalFree           = windows.NewLazySystemDLL("kernel32.dll").NewProc("GlobalFree")
)

// WinInet options and values (wininet.h).
const (
	internetOptionRefresh             = 37
	internetOptionSettingsChanged     = 39
	internetOptionPerConnectionOption = 75

	perConnFlags         = 1
	perConnProxyServer   = 2
	perConnProxyBypass   = 3
	perConnAutoconfigURL = 4

	proxyTypeDirect       = 0x1
	proxyTypeProxy        = 0x2
	proxyTypeAutoProxyURL = 0x4
	proxyTypeAutoDetect   = 0x8
)

// perConnOption is INTERNET_PER_CONN_OPTIONW. Its value is a union of a DWORD,
// a string pointer and a FILETIME; uint64 gives the union's size and alignment
// on every Windows architecture, and the DWORD or pointer sits at its start.
type perConnOption struct {
	option uint32
	value  uint64
}

func (o *perConnOption) dword() uint32 { return uint32(o.value) }

func (o *perConnOption) setDword(v uint32) { o.value = uint64(v) }

func (o *perConnOption) str() **uint16 { return (**uint16)(unsafe.Pointer(&o.value)) }

// perConnOptionList is INTERNET_PER_CONN_OPTION_LISTW. A nil connection names
// the default (LAN) connection.
type perConnOptionList struct {
	size        uint32
	connection  *uint16
	optionCount uint32
	optionError uint32
	options     *perConnOption
}

func newOptionList(opts []perConnOption) perConnOptionList {
	return perConnOptionList{
		size:        uint32(unsafe.Sizeof(perConnOptionList{})),
		optionCount: uint32(len(opts)),
		options:     &opts[0],
	}
}

// wininetBackend is the real winProxyBackend.
type wininetBackend struct{}

func (wininetBackend) read() (WindowsProxyState, error) {
	opts := []perConnOption{
		{option: perConnFlags},
		{option: perConnProxyServer},
		{option: perConnProxyBypass},
		{option: perConnAutoconfigURL},
	}
	list := newOptionList(opts)
	size := list.size
	ret, _, err := procInternetQueryOptionW.Call(0, internetOptionPerConnectionOption,
		uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&size)))
	if ret == 0 {
		return WindowsProxyState{}, fmt.Errorf("InternetQueryOption: %w", err)
	}
	// WinInet allocates each returned string; the caller frees it.
	strs := make([]string, len(opts))
	for i := 1; i < len(opts); i++ {
		if p := *opts[i].str(); p != nil {
			strs[i] = windows.UTF16PtrToString(p)
			procGlobalFree.Call(uintptr(unsafe.Pointer(p)))
		}
	}
	flags := opts[0].dword()
	autoDetect := flags&proxyTypeAutoDetect != 0
	state := WindowsProxyState{
		ProxyEnable:   flags&proxyTypeProxy != 0,
		ProxyServer:   strs[1],
		ProxyOverride: strs[2],
		AutoDetect:    &autoDetect,
	}
	// The URL is kept while "Use setup script" is off; only an enabled one
	// counts, matching the AutoConfigURL registry value earlier captures read.
	if flags&proxyTypeAutoProxyURL != 0 {
		state.AutoConfigURL = strs[3]
	}
	return state, nil
}

func (wininetBackend) write(state WindowsProxyState) error {
	flags := uint32(proxyTypeDirect)
	if state.ProxyEnable {
		flags |= proxyTypeProxy
	}
	if state.AutoConfigURL != "" {
		flags |= proxyTypeAutoProxyURL
	}
	if state.AutoDetect != nil && *state.AutoDetect {
		flags |= proxyTypeAutoDetect
	}
	opts := []perConnOption{
		{option: perConnFlags},
		{option: perConnProxyServer},
		{option: perConnProxyBypass},
		{option: perConnAutoconfigURL},
	}
	opts[0].setDword(flags)
	// The pointers live in uint64 fields the garbage collector does not
	// scan, so strs keeps the strings alive until the call returns.
	strs := make([]*uint16, len(opts))
	for i, s := range []string{"", state.ProxyServer, state.ProxyOverride, state.AutoConfigURL} {
		if s == "" {
			continue
		}
		p, err := windows.UTF16PtrFromString(s)
		if err != nil {
			return fmt.Errorf("encode proxy option %d: %w", opts[i].option, err)
		}
		strs[i] = p
		*opts[i].str() = p
	}
	list := newOptionList(opts)
	ret, _, err := procInternetSetOptionW.Call(0, internetOptionPerConnectionOption,
		uintptr(unsafe.Pointer(&list)), uintptr(list.size))
	runtime.KeepAlive(strs)
	if ret == 0 {
		return fmt.Errorf("InternetSetOption: %w", err)
	}
	return nil
}

func (wininetBackend) notify() error {
	if err := internetSetOption(internetOptionSettingsChanged); err != nil {
		return err
	}
	return internetSetOption(internetOptionRefresh)
}

// internetSetOption calls InternetSetOptionW(NULL, option, NULL, 0): both
// notifications take no buffer. A zero return is failure, with the reason in
// GetLastError (surfaced as the call's error result).
func internetSetOption(option uintptr) error {
	ret, _, err := procInternetSetOptionW.Call(0, option, 0, 0)
	if ret == 0 {
		return fmt.Errorf("InternetSetOption(%d): %w", option, err)
	}
	return nil
}
