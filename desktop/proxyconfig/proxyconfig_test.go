// SPDX-License-Identifier: GPL-3.0-or-later

package proxyconfig

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"openrung/desktop/persist"
)

func TestResolvePortUsesEnvironmentOverrideWithoutPersistingIt(t *testing.T) {
	store := persist.NewInDir(t.TempDir())
	if err := store.SaveProxyPort(41111); err != nil {
		t.Fatal(err)
	}
	resolution, err := resolvePort(store, func(name string) (string, bool) {
		if name != PortEnv {
			t.Fatalf("lookup name = %q", name)
		}
		return " 42222 ", true
	}, func() (int, error) {
		t.Fatal("allocator called despite environment override")
		return 0, nil
	})
	if err != nil || resolution.Port != 42222 || resolution.PersistenceWarning != nil {
		t.Fatalf("resolvePort = %+v, %v; want port 42222 without warning", resolution, err)
	}
	if persisted, ok := store.LoadProxyPort(); !ok || persisted != 41111 {
		t.Fatalf("override changed persisted port: %d, %v", persisted, ok)
	}
}

func TestResolvePortReusesOrCreatesPersistedPort(t *testing.T) {
	dir := t.TempDir()
	store := persist.NewInDir(dir)
	lookup := func(string) (string, bool) { return "", false }
	allocations := 0
	allocate := func() (int, error) {
		allocations++
		return 46685, nil
	}
	first, err := resolvePort(store, lookup, allocate)
	if err != nil || first.Port != 46685 || first.PersistenceWarning != nil {
		t.Fatalf("first resolvePort = %+v, %v", first, err)
	}
	second, err := resolvePort(persist.NewInDir(dir), lookup, func() (int, error) {
		t.Fatal("allocator called despite persisted port")
		return 0, nil
	})
	if err != nil || second.Port != first.Port || second.PersistenceWarning != nil || allocations != 1 {
		t.Fatalf("second resolvePort = %+v, %v; allocations=%d", second, err, allocations)
	}
}

func TestResolvePortKeepsWorkingWhenPersistenceFails(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := persist.NewInDir(filepath.Join(blocker, "openrung"))
	resolution, err := resolvePort(store, func(string) (string, bool) { return "", false }, func() (int, error) {
		return 46685, nil
	})
	if err != nil || resolution.Port != 46685 || resolution.PersistenceWarning == nil {
		t.Fatalf("resolvePort = %+v, %v; want usable port with persistence warning", resolution, err)
	}
}

func TestResolvePortRejectsInvalidOverrideAndAllocator(t *testing.T) {
	for _, value := range []string{"abc", "0", "-1", "65536"} {
		_, err := resolvePort(nil, func(string) (string, bool) { return value, true }, func() (int, error) {
			t.Fatal("allocator called for invalid explicit override")
			return 0, nil
		})
		if err == nil || !strings.Contains(err.Error(), PortEnv) {
			t.Fatalf("override %q error = %v", value, err)
		}
	}
	_, err := resolvePort(nil, func(string) (string, bool) { return "", false }, func() (int, error) {
		return 0, nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid proxy port") {
		t.Fatalf("invalid allocator error = %v", err)
	}
	want := errors.New("listen failed")
	_, err = resolvePort(nil, func(string) (string, bool) { return "", false }, func() (int, error) {
		return 0, want
	})
	if !errors.Is(err, want) {
		t.Fatalf("allocator error = %v, want wrapped %v", err, want)
	}
}

func TestEnsureAvailableReportsStablePortCollision(t *testing.T) {
	listener, err := net.Listen("tcp", Host+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := EnsureAvailable(port); err == nil || !strings.Contains(err.Error(), PortEnv) {
		t.Fatalf("occupied port error = %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := EnsureAvailable(port); err != nil {
		t.Fatalf("released port still unavailable: %v", err)
	}
}

func TestWriteShellHelperQuotesPathAndBuildsEndpoint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "user's config")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info, err := WriteShellHelper(persist.NewInDir(dir), 46685)
	if err != nil {
		t.Fatal(err)
	}
	if info.Host != Host || info.Port != 46685 || info.Endpoint != "127.0.0.1:46685" {
		t.Fatalf("unexpected info: %+v", info)
	}
	if !strings.Contains(info.EnableCommand, `'"'"'`) || !strings.HasSuffix(info.EnableCommand, " && openrung_proxy_on") {
		t.Fatalf("enable command is not safely quoted: %s", info.EnableCommand)
	}
	if info.DisableCommand != "openrung_proxy_off" {
		t.Fatalf("disable command = %q", info.DisableCommand)
	}
}

func TestSanitizeInheritedProxyEnvironmentRemovesOnlyOpenRungValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX proxy variable aliases are case-insensitive on Windows")
	}
	t.Setenv(ShellProxyEnv, "127.0.0.1:46685")
	t.Setenv("http_proxy", "http://127.0.0.1:46685")
	t.Setenv("https_proxy", "http://upstream.example:3128")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:46685")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:46685")
	t.Setenv("all_proxy", "socks5h://127.0.0.1:46685")
	t.Setenv("ALL_PROXY", "socks5h://127.0.0.1:46685")
	t.Setenv("NO_PROXY", "localhost")
	httpLower := shellVariable{name: "http_proxy", saved: "HTTP_PROXY_LOWER"}
	t.Setenv(savedValueName(httpLower), "http://required-upstream.example:3128")
	t.Setenv(savedSetName(httpLower), "1")
	t.Setenv(savedExportedName(httpLower), "1")
	httpUpper := shellVariable{name: "HTTP_PROXY", saved: "HTTP_PROXY_UPPER"}
	t.Setenv(savedValueName(httpUpper), "")
	t.Setenv(savedSetName(httpUpper), "1")
	t.Setenv(savedExportedName(httpUpper), "1")
	allLower := shellVariable{name: "all_proxy", saved: "ALL_PROXY_LOWER"}
	t.Setenv(savedValueName(allLower), "socks5://shell-local.example:1080")
	t.Setenv(savedSetName(allLower), "1")
	t.Setenv(savedExportedName(allLower), "0")

	SanitizeInheritedProxyEnvironment()

	for _, name := range []string{ShellProxyEnv, "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		if value, ok := os.LookupEnv(name); ok {
			t.Errorf("%s remained set to %q", name, value)
		}
	}
	if got := os.Getenv("http_proxy"); got != "http://required-upstream.example:3128" {
		t.Errorf("exported upstream proxy was not restored: %q", got)
	}
	if got, ok := os.LookupEnv("HTTP_PROXY"); !ok || got != "" {
		t.Errorf("exported empty HTTP_PROXY was not restored: value=%q set=%v", got, ok)
	}
	if got := os.Getenv("https_proxy"); got != "http://upstream.example:3128" {
		t.Errorf("unrelated upstream proxy changed to %q", got)
	}
	if got := os.Getenv("NO_PROXY"); got != "localhost" {
		t.Errorf("NO_PROXY changed to %q", got)
	}
	for _, variable := range []shellVariable{httpLower, httpUpper, allLower} {
		for _, name := range []string{savedValueName(variable), savedSetName(variable), savedExportedName(variable)} {
			if _, ok := os.LookupEnv(name); ok {
				t.Errorf("internal saved variable %s was not cleared", name)
			}
		}
	}
}

func TestSanitizeInheritedProxyEnvironmentIgnoresInvalidMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX proxy variable aliases are case-insensitive on Windows")
	}
	t.Setenv(ShellProxyEnv, "not-an-endpoint")
	t.Setenv("http_proxy", "http://127.0.0.1:46685")
	SanitizeInheritedProxyEnvironment()
	if got := os.Getenv("http_proxy"); got != "http://127.0.0.1:46685" {
		t.Fatalf("proxy changed despite invalid marker: %q", got)
	}
	if _, ok := os.LookupEnv(ShellProxyEnv); ok {
		t.Fatal("invalid marker was not cleared")
	}
}

func TestShellHelperPreservesAndRestoresProxyEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell integration is not exposed on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("POSIX sh is unavailable")
	}
	store := persist.NewInDir(t.TempDir())
	info, err := WriteShellHelper(store, 46685)
	if err != nil {
		t.Fatal(err)
	}
	command := `
http_proxy='old http'; export http_proxy
unset https_proxy
HTTP_PROXY=''; export HTTP_PROXY
HTTPS_PROXY='old secure'; export HTTPS_PROXY
unset all_proxy
all_proxy='old local'
ALL_PROXY='old all'; export ALL_PROXY
unset OPENRUNG_SHELL_PROXY

. "$1"
[ "$http_proxy" = 'old http' ]
[ "${https_proxy+x}" != x ]

openrung_proxy_on
[ "$http_proxy" = 'http://127.0.0.1:46685' ]
[ "$https_proxy" = 'http://127.0.0.1:46685' ]
[ "$HTTP_PROXY" = 'http://127.0.0.1:46685' ]
[ "$HTTPS_PROXY" = 'http://127.0.0.1:46685' ]
[ "$all_proxy" = 'socks5h://127.0.0.1:46685' ]
[ "$ALL_PROXY" = 'socks5h://127.0.0.1:46685' ]
[ "$OPENRUNG_SHELL_PROXY" = '127.0.0.1:46685' ]
[ "$_OPENRUNG_SAVED_HTTP_PROXY_LOWER" = 'old http' ]
command env | command grep '^_OPENRUNG_SAVED_HTTP_PROXY_LOWER=old http$' >/dev/null

http_proxy='mutated while active'
openrung_proxy_on
[ "$http_proxy" = 'http://127.0.0.1:46685' ]

openrung_proxy_off
[ "$http_proxy" = 'old http' ]
[ "${https_proxy+x}" != x ]
[ "${HTTP_PROXY+x}" = x ]
[ -z "$HTTP_PROXY" ]
[ "$HTTPS_PROXY" = 'old secure' ]
[ "$all_proxy" = 'old local' ]
if command env | command grep '^all_proxy=' >/dev/null; then exit 1; fi
[ "$ALL_PROXY" = 'old all' ]
[ "${OPENRUNG_SHELL_PROXY+x}" != x ]
command env | command grep '^http_proxy=old http$' >/dev/null
[ "${_OPENRUNG_SAVED_HTTP_PROXY_LOWER+x}" != x ]
`
	cmd := exec.Command("sh", "-eu", "-c", command, "openrung-proxy-test", info.HelperPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell helper failed: %v\n%s", err, output)
	}
}
