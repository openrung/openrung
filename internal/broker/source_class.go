// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Datacenter source classification (ADR-006 "Client enrollment and relay
// authorization"). New user identities are refused from hosting and
// datacenter networks; an existing identity authenticates from anywhere.
//
// The classifier answers from two local files the operator refreshes (see
// deploy/broker/refresh-source-asn.sh), never from a per-request lookup:
//
//   - an IP-to-ASN table in the iptoasn.com TSV layout, one range per line:
//     range_start <TAB> range_end <TAB> asn [<TAB> country <TAB> description];
//   - a list of datacenter ASNs, one per line ("AS64500" or "64500"; anything
//     after the number, '#' comments and lines that start with no number,
//     such as a CSV header, are ignored).
//
// Only ranges whose ASN is on the list are kept in memory, so a lookup is a
// binary search over the datacenter ranges and an address outside them
// (including one the table does not cover) is not a datacenter. The files are
// re-read when either changes; a file that fails to parse leaves the previous
// data in place. The table's modification time dates the data: past MaxAge,
// or before any successful load, classification is unavailable and the
// broker answers enrollment with a retryable error instead of guessing.

// DefaultSourceDataMaxAge is how old the IP-to-ASN table may grow before
// classification is reported unavailable.
const DefaultSourceDataMaxAge = 7 * 24 * time.Hour

// sourceReloadInterval is how often Run checks the files for a change.
const sourceReloadInterval = time.Minute

type sourceClass int

const (
	// sourceUnlisted: not on a datacenter range (or exempt) — enrollment
	// proceeds as before.
	sourceUnlisted sourceClass = iota
	// sourceDatacenter: on a range announced by a listed datacenter ASN.
	sourceDatacenter
	// sourceUnclassified: classification is configured but has no data, or
	// its data is older than MaxAge.
	sourceUnclassified
)

// SourceClassifierConfig names the classifier's inputs.
type SourceClassifierConfig struct {
	// ASNDBPath is the IP-to-ASN table (OPENRUNG_SOURCE_ASN_DB).
	ASNDBPath string
	// DatacenterASNListPath is the datacenter ASN list
	// (OPENRUNG_DATACENTER_ASN_LIST).
	DatacenterASNListPath string
	// ExemptCIDRs are never classified as datacenter sources
	// (OPENRUNG_ENROLLMENT_EXEMPT_CIDRS); malformed entries are an error.
	ExemptCIDRs []string
	// MaxAge bounds the table's age; zero applies DefaultSourceDataMaxAge.
	MaxAge time.Duration
}

// SourceClassifier classifies client source addresses; safe for concurrent
// use. Build one with NewSourceClassifier and keep it fresh with Run.
type SourceClassifier struct {
	cfg    SourceClassifierConfig
	exempt []netip.Prefix

	mu   sync.RWMutex
	data *sourceData
	// stamps are the files' (size, mtime) at the last load attempt, so a
	// file that failed to parse is not re-read until it changes again.
	stamps    [2]fileStamp
	attempted bool
}

type fileStamp struct {
	size    int64
	modTime time.Time
}

// sourceData is one loaded snapshot: the datacenter ranges sorted by start
// and the table's modification time.
type sourceData struct {
	ranges   []addrRange
	asOf     time.Time
	asnCount int
}

type addrRange struct{ start, end netip.Addr }

// NewSourceClassifier validates the configuration and loads the files once.
// A load failure is logged, not returned: the broker still starts and
// answers enrollment with 503 until a later reload succeeds.
func NewSourceClassifier(cfg SourceClassifierConfig) (*SourceClassifier, error) {
	cfg.ASNDBPath = strings.TrimSpace(cfg.ASNDBPath)
	cfg.DatacenterASNListPath = strings.TrimSpace(cfg.DatacenterASNListPath)
	if cfg.ASNDBPath == "" || cfg.DatacenterASNListPath == "" {
		return nil, errors.New("datacenter source classification needs both OPENRUNG_SOURCE_ASN_DB and OPENRUNG_DATACENTER_ASN_LIST")
	}
	if cfg.MaxAge < 0 {
		return nil, errors.New("OPENRUNG_SOURCE_ASN_MAX_AGE must be a positive duration like 168h")
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = DefaultSourceDataMaxAge
	}
	c := &SourceClassifier{cfg: cfg}
	for _, raw := range cfg.ExemptCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("OPENRUNG_ENROLLMENT_EXEMPT_CIDRS: %w", err)
		}
		c.exempt = append(c.exempt, prefix.Masked())
	}
	c.Reload()
	return c, nil
}

// Run re-checks the files every interval (zero: one minute) until ctx ends.
func (c *SourceClassifier) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = sourceReloadInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Reload()
		}
	}
}

// Reload re-reads both files when either changed since the last attempt and
// reports whether new data was installed.
func (c *SourceClassifier) Reload() bool {
	var stamps [2]fileStamp
	for i, path := range []string{c.cfg.ASNDBPath, c.cfg.DatacenterASNListPath} {
		info, err := os.Stat(path)
		if err != nil {
			// Logged on the first attempt and when a file disappears, not
			// on every check while it stays missing.
			c.mu.Lock()
			report := !c.attempted || c.stamps != [2]fileStamp{}
			c.attempted, c.stamps = true, [2]fileStamp{}
			c.mu.Unlock()
			if report {
				slog.Error("datacenter source classification data unavailable", "error", err)
			}
			return false
		}
		stamps[i] = fileStamp{size: info.Size(), modTime: info.ModTime()}
	}
	c.mu.Lock()
	unchanged := c.attempted && stamps == c.stamps
	c.attempted, c.stamps = true, stamps
	c.mu.Unlock()
	if unchanged {
		return false
	}

	data, err := loadSourceData(c.cfg.ASNDBPath, c.cfg.DatacenterASNListPath)
	if err != nil {
		slog.Error("could not load datacenter source classification data; keeping the previous data", "error", err)
		return false
	}
	data.asOf = stamps[0].modTime
	c.mu.Lock()
	c.data = data
	c.mu.Unlock()
	slog.Info("datacenter source classification data loaded",
		"datacenter_asns", data.asnCount, "datacenter_ranges", len(data.ranges), "as_of", data.asOf.UTC().Format(time.RFC3339))
	return true
}

func (c *SourceClassifier) loaded() *sourceData {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data
}

// Available reports whether classification has data no older than MaxAge.
func (c *SourceClassifier) Available(now time.Time) bool {
	data := c.loaded()
	return data != nil && now.Sub(data.asOf) <= c.cfg.MaxAge
}

// classify places a source address. Exempt addresses are unlisted whatever
// the data says; an unparseable source is unlisted too (classification
// covers addresses, and the client-IP resolver only yields addresses).
func (c *SourceClassifier) classify(source string, now time.Time) sourceClass {
	addr, err := netip.ParseAddr(strings.TrimSpace(source))
	if err != nil {
		return sourceUnlisted
	}
	addr = addr.Unmap().WithZone("")
	for _, prefix := range c.exempt {
		if prefix.Contains(addr) {
			return sourceUnlisted
		}
	}
	data := c.loaded()
	if data == nil || now.Sub(data.asOf) > c.cfg.MaxAge {
		return sourceUnclassified
	}
	if data.contains(addr) {
		return sourceDatacenter
	}
	return sourceUnlisted
}

// contains finds the last range starting at or before addr and checks its
// end. Ranges never span address families (Compare orders every IPv4
// address before every IPv6 one), so one sorted slice serves both.
func (d *sourceData) contains(addr netip.Addr) bool {
	i, found := slices.BinarySearchFunc(d.ranges, addr, func(r addrRange, a netip.Addr) int { return r.start.Compare(a) })
	if found {
		return true
	}
	if i == 0 {
		return false
	}
	return addr.Compare(d.ranges[i-1].end) <= 0
}

func loadSourceData(tablePath, listPath string) (*sourceData, error) {
	asns, err := readDatacenterASNs(listPath)
	if err != nil {
		return nil, err
	}
	ranges, err := readDatacenterRanges(tablePath, asns)
	if err != nil {
		return nil, err
	}
	return &sourceData{ranges: ranges, asnCount: len(asns)}, nil
}

// readDatacenterASNs parses the ASN list. It must name at least one ASN: an
// empty list would silently classify every source as unlisted.
func readDatacenterASNs(path string) (map[uint32]bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	asns := make(map[uint32]bool)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) >= 2 && strings.EqualFold(line[:2], "AS") {
			line = line[2:]
		}
		end := 0
		for end < len(line) && line[end] >= '0' && line[end] <= '9' {
			end++
		}
		if end == 0 {
			continue // blank, comment or header
		}
		asn, err := strconv.ParseUint(line[:end], 10, 32)
		if err != nil || asn == 0 {
			continue
		}
		asns[uint32(asn)] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read datacenter ASN list: %w", err)
	}
	if len(asns) == 0 {
		return nil, errors.New("datacenter ASN list names no ASN")
	}
	return asns, nil
}

// readDatacenterRanges parses the IP-to-ASN table strictly — any malformed
// line rejects the whole file, so a truncated download never installs — and
// keeps the ranges announced by a listed ASN, sorted by start.
func readDatacenterRanges(path string, asns map[uint32]bool) ([]addrRange, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var ranges []addrRange
	rows := 0
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		fields := strings.SplitN(text, "\t", 4)
		if len(fields) < 3 {
			return nil, fmt.Errorf("IP-to-ASN table line %d: want range_start, range_end and asn", line)
		}
		start, errStart := netip.ParseAddr(fields[0])
		end, errEnd := netip.ParseAddr(fields[1])
		asn, errASN := strconv.ParseUint(fields[2], 10, 32)
		if errStart != nil || errEnd != nil || errASN != nil {
			return nil, fmt.Errorf("IP-to-ASN table line %d: malformed range or asn", line)
		}
		start, end = start.Unmap(), end.Unmap()
		if start.Is4() != end.Is4() || end.Less(start) {
			return nil, fmt.Errorf("IP-to-ASN table line %d: invalid range", line)
		}
		rows++
		if asns[uint32(asn)] {
			ranges = append(ranges, addrRange{start: start, end: end})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read IP-to-ASN table: %w", err)
	}
	if rows == 0 {
		return nil, errors.New("IP-to-ASN table is empty")
	}
	slices.SortFunc(ranges, func(a, b addrRange) int { return a.start.Compare(b.start) })
	return ranges, nil
}
