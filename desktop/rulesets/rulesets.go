// Package rulesets embeds and stages the binary sing-box rule sets used by the
// desktop split-tunneling country presets.
//
// Staging and validation deliberately fail toward the proxy: a country is
// returned only when both of its pinned files are readable and intact. Callers
// may log Result.Warnings, but must use Result.Countries as the authoritative
// list instead of failing a connection because a preset is unavailable.
package rulesets

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	CountryIran  = "ir"
	CountryChina = "cn"
)

type asset struct {
	name   string
	sha256 string
}

var countryAssets = map[string][2]asset{
	CountryIran: {
		{name: "geosite-ir.srs", sha256: "22add255a0ea2fccc799a0c45508df5b67319d9d2c30ed2ad37bfa4e6d67ce81"},
		{name: "geoip-ir.srs", sha256: "36d46ea40dfe65d722ee4a4171bc93db8ad6f5dd75265ffb448979761ece9c53"},
	},
	CountryChina: {
		{name: "geosite-cn.srs", sha256: "a0dba9663dd160836106740198ed2ce78aa348946e50e5f5666e9a8b7c4097e4"},
		{name: "geoip-cn.srs", sha256: "bc1a9eb66f9c6a0fe9fc5300cf5b5e885e0f9eadd7213b085b767a95d6af3d2a"},
	},
}

var allAssets = []asset{
	countryAssets[CountryIran][0],
	countryAssets[CountryIran][1],
	countryAssets[CountryChina][0],
	countryAssets[CountryChina][1],
}

//go:embed dist/*.srs
var bundled embed.FS

// Result describes the usable subset of a requested split-tunnel selection.
// Countries and Dropped always use canonical ir,cn order. Warnings are
// diagnostics only: an unavailable preset is intentionally not a fatal error.
type Result struct {
	Directory string
	Countries []string
	Dropped   []string
	Warnings  []error
}

// CanonicalCountries normalizes a requested selection to the supported
// lowercase country codes, removing duplicates and unknown values and always
// returning Iran before China.
func CanonicalCountries(requested []string) []string {
	wanted := make(map[string]bool, len(requested))
	for _, country := range requested {
		wanted[strings.ToLower(strings.TrimSpace(country))] = true
	}

	canonical := make([]string, 0, len(countryAssets))
	for _, country := range []string{CountryIran, CountryChina} {
		if wanted[country] {
			canonical = append(canonical, country)
		}
	}
	return canonical
}

// Stage writes the requested countries' bundled assets into directory and
// validates each pair. Writes use a temporary file in the destination
// directory plus an atomic replacement, so sing-box can never observe a
// partially written rule set. A staging or validation failure drops only the
// affected requested country and is reported in Result.Warnings.
func Stage(directory string, requested []string) Result {
	canonical := CanonicalCountries(requested)
	result := emptyResult(directory, canonical)

	absDirectory, err := absoluteDirectory(directory)
	if err != nil {
		result.Warnings = append(result.Warnings, err)
		return result
	}
	result.Directory = absDirectory

	if err := os.MkdirAll(absDirectory, 0o755); err != nil {
		result.Warnings = append(result.Warnings, fmt.Errorf("create rule-set directory %q: %w", absDirectory, err))
		return result
	}

	for _, country := range canonical {
		for _, bundledAsset := range countryAssets[country] {
			if err := stageAsset(absDirectory, bundledAsset); err != nil {
				result.Warnings = append(result.Warnings, err)
			}
		}
	}

	validated := Validate(absDirectory, canonical)
	result.Countries = validated.Countries
	result.Dropped = validated.Dropped
	result.Warnings = append(result.Warnings, validated.Warnings...)
	return result
}

// Validate inspects an existing on-disk directory without modifying it. A
// requested country survives only when both of its files are regular, readable,
// and match the hashes pinned alongside the embedded copies.
func Validate(directory string, requested []string) Result {
	canonical := CanonicalCountries(requested)
	result := emptyResult(directory, canonical)

	absDirectory, err := absoluteDirectory(directory)
	if err != nil {
		result.Warnings = append(result.Warnings, err)
		return result
	}
	result.Directory = absDirectory

	result.Countries = make([]string, 0, len(canonical))
	result.Dropped = result.Dropped[:0]
	for _, country := range canonical {
		pairAvailable := true
		for _, required := range countryAssets[country] {
			path := filepath.Join(absDirectory, required.name)
			if err := validateFile(path, required.sha256); err != nil {
				pairAvailable = false
				result.Warnings = append(result.Warnings, fmt.Errorf("validate %s split-tunnel rule set %q: %w", country, path, err))
			}
		}
		if pairAvailable {
			result.Countries = append(result.Countries, country)
		} else {
			result.Dropped = append(result.Dropped, country)
		}
	}
	return result
}

func emptyResult(directory string, canonical []string) Result {
	return Result{
		Directory: directory,
		Countries: []string{},
		Dropped:   append([]string(nil), canonical...),
		Warnings:  []error{},
	}
}

func absoluteDirectory(directory string) (string, error) {
	if strings.TrimSpace(directory) == "" {
		return "", errors.New("rule-set directory is required")
	}
	absDirectory, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve rule-set directory %q: %w", directory, err)
	}
	return filepath.Clean(absDirectory), nil
}

func stageAsset(directory string, bundledAsset asset) error {
	target := filepath.Join(directory, bundledAsset.name)
	if validateFile(target, bundledAsset.sha256) == nil {
		return nil
	}

	contents, err := bundled.ReadFile(filepath.ToSlash(filepath.Join("dist", bundledAsset.name)))
	if err != nil {
		return fmt.Errorf("read embedded rule set %q: %w", bundledAsset.name, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(contents)); got != bundledAsset.sha256 {
		return fmt.Errorf("embedded rule set %q has sha256 %s, want %s", bundledAsset.name, got, bundledAsset.sha256)
	}

	temp, err := os.CreateTemp(directory, "."+bundledAsset.name+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary rule set for %q: %w", target, err)
	}
	tempName := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempName)
	}()

	if err := temp.Chmod(0o644); err != nil {
		return fmt.Errorf("set permissions on temporary rule set %q: %w", tempName, err)
	}
	if _, err := io.Copy(temp, bytes.NewReader(contents)); err != nil {
		return fmt.Errorf("write temporary rule set %q: %w", tempName, err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary rule set %q: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary rule set %q: %w", tempName, err)
	}
	closed = true
	if err := replaceFile(tempName, target); err != nil {
		return fmt.Errorf("publish rule set %q: %w", target, err)
	}
	if err := validateFile(target, bundledAsset.sha256); err != nil {
		return fmt.Errorf("verify staged rule set %q: %w", target, err)
	}
	return nil
}

func validateFile(path, expectedSHA256 string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file (mode %s)", info.Mode())
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if got := fmt.Sprintf("%x", hash.Sum(nil)); got != expectedSHA256 {
		return fmt.Errorf("sha256 %s, want %s", got, expectedSHA256)
	}
	return nil
}
