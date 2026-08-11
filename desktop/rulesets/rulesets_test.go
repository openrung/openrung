package rulesets

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCanonicalCountries(t *testing.T) {
	got := CanonicalCountries([]string{"CN", "ir", "cn", " IR ", "us", ""})
	want := []string{CountryIran, CountryChina}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CanonicalCountries() = %v, want %v", got, want)
	}
}

func TestStageWritesAndValidatesRequestedPairs(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "rulesets")
	result := Stage(directory, []string{"cn", "ir", "cn"})

	assertCountries(t, result.Countries, []string{CountryIran, CountryChina})
	assertCountries(t, result.Dropped, []string{})
	if len(result.Warnings) != 0 {
		t.Fatalf("Stage() warnings = %v, want none", result.Warnings)
	}
	if !filepath.IsAbs(result.Directory) {
		t.Fatalf("Stage() directory = %q, want absolute path", result.Directory)
	}

	for _, bundledAsset := range allAssets {
		path := filepath.Join(result.Directory, bundledAsset.name)
		if err := validateFile(path, bundledAsset.sha256); err != nil {
			t.Errorf("staged %s: %v", bundledAsset.name, err)
		}
	}
	entries, err := os.ReadDir(result.Directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("temporary staging file was left behind: %s", entry.Name())
		}
	}
}

func TestStageWritesOnlyRequestedCountryAssets(t *testing.T) {
	result := Stage(t.TempDir(), []string{"ir"})
	assertCountries(t, result.Countries, []string{CountryIran})
	if len(result.Warnings) != 0 {
		t.Fatalf("Stage() warnings = %v, want none", result.Warnings)
	}
	for _, required := range countryAssets[CountryIran] {
		if _, err := os.Stat(filepath.Join(result.Directory, required.name)); err != nil {
			t.Fatalf("requested asset %s was not staged: %v", required.name, err)
		}
	}
	for _, unrequested := range countryAssets[CountryChina] {
		if _, err := os.Stat(filepath.Join(result.Directory, unrequested.name)); !os.IsNotExist(err) {
			t.Fatalf("unrequested asset %s unexpectedly staged (err=%v)", unrequested.name, err)
		}
	}
}

func TestStageRepairsCorruptAsset(t *testing.T) {
	directory := t.TempDir()
	corruptPath := filepath.Join(directory, "geosite-ir.srs")
	if err := os.WriteFile(corruptPath, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := Stage(directory, []string{"ir"})
	assertCountries(t, result.Countries, []string{CountryIran})
	assertCountries(t, result.Dropped, []string{})
	if len(result.Warnings) != 0 {
		t.Fatalf("Stage() warnings = %v, want none after repair", result.Warnings)
	}
	if err := validateFile(corruptPath, countryAssets[CountryIran][0].sha256); err != nil {
		t.Fatalf("repaired asset: %v", err)
	}
}

func TestConcurrentStagePublishesOnlyIntactAssets(t *testing.T) {
	directory := t.TempDir()
	const writers = 6
	results := make(chan Result, writers)
	var group sync.WaitGroup
	for range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- Stage(directory, []string{"ir", "cn"})
		}()
	}
	group.Wait()
	close(results)

	for result := range results {
		assertCountries(t, result.Countries, []string{CountryIran, CountryChina})
		if len(result.Warnings) != 0 {
			t.Errorf("concurrent Stage() warnings = %v", result.Warnings)
		}
	}
	validated := Validate(directory, []string{"ir", "cn"})
	assertCountries(t, validated.Countries, []string{CountryIran, CountryChina})
	if len(validated.Warnings) != 0 {
		t.Fatalf("Validate() after concurrent staging warnings = %v", validated.Warnings)
	}
}

func TestValidateDropsOnlyCountriesWithAnUnavailableHalf(t *testing.T) {
	directory := t.TempDir()
	staged := Stage(directory, []string{"ir", "cn"})
	if len(staged.Warnings) != 0 {
		t.Fatalf("initial Stage() warnings = %v", staged.Warnings)
	}
	if err := os.Remove(filepath.Join(directory, "geoip-cn.srs")); err != nil {
		t.Fatal(err)
	}

	result := Validate(directory, []string{"CN", "IR"})
	assertCountries(t, result.Countries, []string{CountryIran})
	assertCountries(t, result.Dropped, []string{CountryChina})
	if len(result.Warnings) != 1 {
		t.Fatalf("Validate() warnings = %v, want one missing-file warning", result.Warnings)
	}
}

func TestValidateRequiresIntactFilesNotJustNames(t *testing.T) {
	directory := t.TempDir()
	staged := Stage(directory, []string{"ir", "cn"})
	if len(staged.Warnings) != 0 {
		t.Fatalf("initial Stage() warnings = %v", staged.Warnings)
	}
	if err := os.WriteFile(filepath.Join(directory, "geosite-ir.srs"), []byte("not an srs"), 0o644); err != nil {
		t.Fatal(err)
	}

	result := Validate(directory, []string{"ir", "cn"})
	assertCountries(t, result.Countries, []string{CountryChina})
	assertCountries(t, result.Dropped, []string{CountryIran})
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0].Error(), "sha256") {
		t.Fatalf("Validate() warnings = %v, want one hash warning", result.Warnings)
	}
}

func TestEmptyDirectoryFailsOpen(t *testing.T) {
	result := Stage("", []string{"ir", "cn"})
	assertCountries(t, result.Countries, []string{})
	assertCountries(t, result.Dropped, []string{CountryIran, CountryChina})
	if len(result.Warnings) != 1 {
		t.Fatalf("Stage() warnings = %v, want one directory warning", result.Warnings)
	}
}

func TestEmbeddedAssetsMatchPinnedHashes(t *testing.T) {
	for _, bundledAsset := range allAssets {
		contents, err := bundled.ReadFile(filepath.ToSlash(filepath.Join("dist", bundledAsset.name)))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(contents)); got != bundledAsset.sha256 {
			t.Errorf("%s sha256 = %s, want %s", bundledAsset.name, got, bundledAsset.sha256)
		}
	}
}

func assertCountries(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("countries = %#v, want %#v", got, want)
	}
}

// TestValidateRejectsFifoWithoutBlocking guards the connect path. os.Open on a
// FIFO blocks until a writer appears, and Stage/Validate run synchronously while
// the service holds connectMu, so opening before the regular-file check would let
// one mkfifo in the cache directory wedge Connect forever.
func TestValidateRejectsFifoWithoutBlocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no FIFOs on windows")
	}
	directory := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(directory, "geosite-ir.srs"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan Result, 1)
	go func() { done <- Validate(directory, []string{"ir"}) }()

	select {
	case result := <-done:
		if len(result.Countries) != 0 {
			t.Fatalf("a FIFO must not validate: %+v", result.Countries)
		}
		if !reflect.DeepEqual(result.Dropped, []string{"ir"}) {
			t.Fatalf("dropped = %v, want [ir]", result.Dropped)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Validate blocked on a FIFO; the connect path would hang holding connectMu")
	}
}

// TestStageRejectsSymlinkedTarget keeps a pre-planted symlink from being treated
// as a staged rule set, and from being written through to a path outside the
// cache directory.
func TestStageRejectsSymlinkedTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.srs")
	if err := os.WriteFile(outside, []byte("not a rule set"), 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "geosite-ir.srs")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Staging replaces the symlink with the real embedded asset rather than
	// following it, so Iran still ends up usable and the outside file is intact.
	result := Stage(directory, []string{"ir"})
	if !reflect.DeepEqual(result.Countries, []string{"ir"}) {
		t.Fatalf("countries = %v, want [ir]; warnings=%v", result.Countries, result.Warnings)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "not a rule set" {
		t.Fatalf("staging wrote through the symlink: data=%q err=%v", data, err)
	}
	info, err := os.Lstat(filepath.Join(directory, "geosite-ir.srs"))
	if err != nil {
		t.Fatalf("lstat staged asset: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("staged asset is still not a regular file: %s", info.Mode())
	}
}
