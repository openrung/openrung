package persist

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"openrung/desktop/proxymode"
)

func TestRecentsRoundTrip(t *testing.T) {
	store := NewInDir(t.TempDir())
	if got := store.LoadRecents(); len(got) != 0 {
		t.Fatalf("fresh store should have no recents, got %v", got)
	}
	recents := []RecentNode{{CountryCode: "JP", Label: "Tokyo, Japan", Latitude: 35.6, Longitude: 139.7}}
	if err := store.SaveRecents(recents); err != nil {
		t.Fatalf("SaveRecents: %v", err)
	}
	got := store.LoadRecents()
	if len(got) != 1 || got[0].CountryCode != "JP" || got[0].Label != "Tokyo, Japan" {
		t.Fatalf("recents round-trip mismatch: %+v", got)
	}
}

func TestProxyPortLifecycle(t *testing.T) {
	dir := t.TempDir()
	store := NewInDir(dir)
	if _, ok := store.LoadProxyPort(); ok {
		t.Fatal("fresh store should have no proxy port")
	}
	if err := store.SaveProxyPort(46685); err != nil {
		t.Fatalf("SaveProxyPort: %v", err)
	}
	if got, ok := store.LoadProxyPort(); !ok || got != 46685 {
		t.Fatalf("LoadProxyPort = %d, %v; want 46685, true", got, ok)
	}
	for _, port := range []int{-1, 0, 65536} {
		if err := store.SaveProxyPort(port); err == nil {
			t.Fatalf("SaveProxyPort(%d) unexpectedly succeeded", port)
		}
	}

	path := filepath.Join(dir, proxyPortFile)
	for _, data := range []string{"not json", `{"port":0}`, `{"port":65536}`} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("write invalid proxy port: %v", err)
		}
		if _, ok := store.LoadProxyPort(); ok {
			t.Fatalf("LoadProxyPort accepted %q", data)
		}
	}
}

func TestLoadOrSaveProxyPortChoosesOneCrossProcessWinner(t *testing.T) {
	if dir := os.Getenv("OPENRUNG_TEST_PROXY_PORT_DIR"); dir != "" {
		candidate, err := strconv.Atoi(os.Getenv("OPENRUNG_TEST_PROXY_PORT_CANDIDATE"))
		if err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(2)
		}
		port, err := NewInDir(dir).LoadOrSaveProxyPort(candidate)
		if err != nil {
			fmt.Fprint(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Print(port)
		os.Exit(0)
	}

	store := NewInDir(t.TempDir())
	const callers = 20
	type child struct {
		cmd    *exec.Cmd
		output bytes.Buffer
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	children := make([]*child, 0, callers)
	for i := 0; i < callers; i++ {
		proc := &child{cmd: exec.Command(executable, "-test.run=^TestLoadOrSaveProxyPortChoosesOneCrossProcessWinner$")}
		proc.cmd.Env = append(os.Environ(),
			"OPENRUNG_TEST_PROXY_PORT_DIR="+store.dir,
			fmt.Sprintf("OPENRUNG_TEST_PROXY_PORT_CANDIDATE=%d", 40000+i),
		)
		proc.cmd.Stdout = &proc.output
		proc.cmd.Stderr = &proc.output
		if err := proc.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		children = append(children, proc)
	}
	ports := make([]int, 0, callers)
	for _, proc := range children {
		if err := proc.cmd.Wait(); err != nil {
			t.Fatalf("proxy-port child: %v: %s", err, proc.output.String())
		}
		port, err := strconv.Atoi(strings.TrimSpace(proc.output.String()))
		if err != nil {
			t.Fatalf("parse child port %q: %v", proc.output.String(), err)
		}
		ports = append(ports, port)
	}
	winner, ok := store.LoadProxyPort()
	if !ok {
		t.Fatal("proxy port was not persisted")
	}
	for _, port := range ports {
		if port != winner {
			t.Fatalf("caller selected %d, persisted winner is %d", port, winner)
		}
	}
}

func TestProxyEnvScriptConcurrentRefreshesAreAtomic(t *testing.T) {
	store := NewInDir(t.TempDir())
	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := store.SaveProxyEnvScript(46685, []byte(fmt.Sprintf("writer-%d\n", index)))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent helper refresh: %v", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(store.dir, fmt.Sprintf(proxyEnvFile, 46685)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "writer-") || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("helper contains a partial write: %q", data)
	}
}

func TestProxyEnvScriptIsPrivateAndReplaceable(t *testing.T) {
	store := NewInDir(t.TempDir())
	path, err := store.SaveProxyEnvScript(46685, []byte("first\n"))
	if err != nil {
		t.Fatalf("SaveProxyEnvScript first: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod helper: %v", err)
	}
	path, err = store.SaveProxyEnvScript(46685, []byte("second\n"))
	if err != nil {
		t.Fatalf("SaveProxyEnvScript replacement: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if string(data) != "second\n" {
		t.Fatalf("helper content = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat helper: %v", err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("helper mode = %o, want 600", got)
	}
	otherPath, err := store.SaveProxyEnvScript(46686, []byte("other\n"))
	if err != nil {
		t.Fatalf("SaveProxyEnvScript other port: %v", err)
	}
	if otherPath == path {
		t.Fatal("different ports shared one mutable shell helper")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "second\n" {
		t.Fatalf("other port rewrote first helper: data=%q err=%v", data, err)
	}
}

func TestPrependRecentDedupesAndCaps(t *testing.T) {
	var recents []RecentNode
	add := func(cc string) {
		recents = PrependRecent(recents, RecentNode{CountryCode: cc, Label: cc}, 3)
	}
	add("JP")
	add("SG")
	add("US")
	add("DE") // exceeds cap 3 → oldest (JP) drops
	if len(recents) != 3 {
		t.Fatalf("expected cap 3, got %d: %+v", len(recents), recents)
	}
	if recents[0].CountryCode != "DE" {
		t.Fatalf("newest should be first, got %q", recents[0].CountryCode)
	}
	// Re-adding an existing code moves it to front without duplicating.
	add("US")
	if len(recents) != 3 {
		t.Fatalf("dedupe failed, len=%d: %+v", len(recents), recents)
	}
	if recents[0].CountryCode != "US" {
		t.Fatalf("re-added code should move to front, got %q", recents[0].CountryCode)
	}
	seen := map[string]int{}
	for _, r := range recents {
		seen[r.CountryCode]++
		if seen[r.CountryCode] > 1 {
			t.Fatalf("duplicate country code %q", r.CountryCode)
		}
	}
}

func TestProxySnapshotLifecycle(t *testing.T) {
	store := NewInDir(t.TempDir())
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("fresh store should have no proxy snapshot")
	}
	snap := proxymode.Snapshot{
		Platform: "darwin",
		Services: []proxymode.ServiceProxyState{{Name: "Wi-Fi", WebEnabled: true, WebHost: "10.0.0.1", WebPort: 3128}},
	}
	if err := store.SaveProxySnapshot(snap); err != nil {
		t.Fatalf("SaveProxySnapshot: %v", err)
	}
	loaded, ok := store.LoadProxySnapshot()
	if !ok || loaded.Platform != "darwin" || len(loaded.Services) != 1 {
		t.Fatalf("snapshot round-trip mismatch: ok=%v %+v", ok, loaded)
	}
	if err := store.ClearProxySnapshot(); err != nil {
		t.Fatalf("ClearProxySnapshot: %v", err)
	}
	if _, ok := store.LoadProxySnapshot(); ok {
		t.Fatal("snapshot should be gone after clear")
	}
	// Clearing a missing snapshot is not an error.
	if err := store.ClearProxySnapshot(); err != nil {
		t.Fatalf("ClearProxySnapshot on missing: %v", err)
	}
}
