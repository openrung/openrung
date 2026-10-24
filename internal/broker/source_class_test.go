// SPDX-License-Identifier: GPL-3.0-or-later

package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// Test data uses documentation prefixes (RFC 5737, RFC 3849) and private-use
// ASNs (RFC 6996) only.
const (
	testASNTable = "192.0.2.0\t192.0.2.255\t64500\tZZ\tEXAMPLE-DC\n" +
		"198.51.100.0\t198.51.100.127\t64501\tZZ\tEXAMPLE-ACCESS\n" +
		"198.51.100.128\t198.51.100.255\t64502\tZZ\tEXAMPLE-HOSTING\n" +
		"203.0.113.0\t203.0.113.255\t64501\tZZ\tEXAMPLE-ACCESS\n" +
		"2001:db8::\t2001:db8:0:ffff:ffff:ffff:ffff:ffff\t64500\tZZ\tEXAMPLE-DC\n" +
		"2001:db8:1::\t2001:db8:1:ffff:ffff:ffff:ffff:ffff\t64501\tZZ\tEXAMPLE-ACCESS\n"
	// The list mixes the CSV and "AS<n> # comment" spellings of the
	// published lists; the header and comment lines name no ASN.
	testDatacenterASNs = "ASN,Entity\n64500,\"Example DC\"\n# operator additions\nAS64502 # example hosting\n\n"
)

type sourceFiles struct {
	dir, table, list string
}

func writeSourceFiles(t *testing.T, table, list string) sourceFiles {
	t.Helper()
	dir := t.TempDir()
	files := sourceFiles{dir: dir, table: filepath.Join(dir, "ip2asn.tsv"), list: filepath.Join(dir, "datacenter-asns.txt")}
	files.write(t, table, list)
	return files
}

// write replaces the files' contents; an empty argument leaves that file.
func (f sourceFiles) write(t *testing.T, table, list string) {
	t.Helper()
	if table != "" {
		if err := os.WriteFile(f.table, []byte(table), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if list != "" {
		if err := os.WriteFile(f.list, []byte(list), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// touch sets the table's modification time, which dates the data.
func (f sourceFiles) touch(t *testing.T, when time.Time) {
	t.Helper()
	if err := os.Chtimes(f.table, when, when); err != nil {
		t.Fatal(err)
	}
}

func newTestClassifier(t *testing.T, files sourceFiles, exempt ...string) *SourceClassifier {
	t.Helper()
	c, err := NewSourceClassifier(SourceClassifierConfig{ASNDBPath: files.table, DatacenterASNListPath: files.list, ExemptCIDRs: exempt})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSourceClassifierLookup(t *testing.T) {
	files := writeSourceFiles(t, testASNTable, testDatacenterASNs)
	c := newTestClassifier(t, files, "192.0.2.128/25")
	now := time.Now()
	for _, tc := range []struct {
		source string
		want   sourceClass
	}{
		{"192.0.2.0", sourceDatacenter},             // first address of a listed range
		{"192.0.2.127", sourceDatacenter},           // last address before the exempt half
		{"192.0.2.128", sourceUnlisted},             // exempt CIDR wins over the data
		{"192.0.2.255", sourceUnlisted},             // last address, exempt
		{"192.0.1.255", sourceUnlisted},             // just below the range: not in the table
		{"198.51.100.127", sourceUnlisted},          // unlisted ASN, adjacent to a listed range
		{"198.51.100.128", sourceDatacenter},        // listed through the "AS<n>" spelling
		{"198.51.100.255", sourceDatacenter},        // last address of the table
		{"198.51.101.0", sourceUnlisted},            // past every range
		{"203.0.113.10", sourceUnlisted},            // consumer range
		{"::ffff:198.51.100.200", sourceDatacenter}, // v4-mapped IPv6 is the IPv4 address
		{"2001:db8::", sourceDatacenter},
		{"2001:db8:0:ffff:ffff:ffff:ffff:ffff", sourceDatacenter},
		{"2001:db8:1::1", sourceUnlisted},
		{"2001:db7:ffff:ffff:ffff:ffff:ffff:ffff", sourceUnlisted},
		{"2001:db8:2::1", sourceUnlisted},
		{"10.0.0.1", sourceUnlisted}, // absent from the table: unknown is not a datacenter
		{"not-an-ip", sourceUnlisted},
	} {
		if got := c.classify(tc.source, now); got != tc.want {
			t.Errorf("classify(%s) = %d, want %d", tc.source, got, tc.want)
		}
	}
	if !c.Available(now) {
		t.Fatal("fresh data reported unavailable")
	}
}

func TestSourceClassifierReloadsOnChangeAndKeepsDataOnBadFiles(t *testing.T) {
	files := writeSourceFiles(t, testASNTable, testDatacenterASNs)
	c := newTestClassifier(t, files)
	now := time.Now()
	if c.Reload() {
		t.Fatal("reloaded unchanged files")
	}

	// Listing another ASN is picked up on the next check.
	files.write(t, "", testDatacenterASNs+"64501\n")
	if !c.Reload() {
		t.Fatal("did not reload a changed ASN list")
	}
	if got := c.classify("203.0.113.10", now); got != sourceDatacenter {
		t.Fatalf("after reload: %d, want datacenter", got)
	}

	// A truncated table (a line without its ASN) is rejected as a whole and
	// the previous data stays in place.
	files.write(t, testASNTable+"2001:db8:9::\t2001:db8:9::ff\n", "")
	if c.Reload() {
		t.Fatal("installed a malformed table")
	}
	if got := c.classify("203.0.113.10", now); got != sourceDatacenter {
		t.Fatalf("after a bad table: %d, want the previous data", got)
	}
	// An empty ASN list is refused too: it would classify nothing.
	files.write(t, testASNTable, "ASN,Entity\n")
	if c.Reload() {
		t.Fatal("installed an empty ASN list")
	}
	// A failed load is not retried until the files change again; fixing
	// them installs the new data.
	if c.Reload() {
		t.Fatal("re-read unchanged bad files")
	}
	files.write(t, "", testDatacenterASNs)
	if !c.Reload() || c.classify("203.0.113.10", now) != sourceUnlisted {
		t.Fatal("fixed files were not installed")
	}
}

func TestSourceClassifierUnavailableAndStale(t *testing.T) {
	now := time.Now()
	missing := sourceFiles{table: filepath.Join(t.TempDir(), "absent.tsv"), list: filepath.Join(t.TempDir(), "absent.txt")}
	c := newTestClassifier(t, missing, "192.0.2.0/24")
	if got := c.classify("203.0.113.10", now); got != sourceUnclassified {
		t.Fatalf("no data: %d, want unclassified", got)
	}
	if got := c.classify("192.0.2.1", now); got != sourceUnlisted {
		t.Fatalf("exempt source without data: %d, want unlisted", got)
	}
	if c.Available(now) {
		t.Fatal("no data reported available")
	}

	files := writeSourceFiles(t, testASNTable, testDatacenterASNs)
	files.touch(t, now.Add(-DefaultSourceDataMaxAge+time.Hour))
	c = newTestClassifier(t, files)
	if got := c.classify("203.0.113.10", now); got != sourceUnlisted {
		t.Fatalf("data within MaxAge: %d", got)
	}
	if got := c.classify("203.0.113.10", now.Add(2*time.Hour)); got != sourceUnclassified {
		t.Fatalf("data past MaxAge: %d, want unclassified", got)
	}
	// A refresh (new modification time) makes it current again.
	files.touch(t, now)
	if !c.Reload() || c.classify("203.0.113.10", now.Add(2*time.Hour)) != sourceUnlisted {
		t.Fatal("refreshed data was not picked up")
	}
}

// The broker side of the refresh job's validation (the operator's refresh
// script refuses the same lines): any one
// malformed line rejects the whole table.
func TestReadDatacenterRangesRejectsMalformedTables(t *testing.T) {
	valid := "192.0.2.0\t192.0.2.127\t64500\tZZ\tA\n2001:db8::\t2001:db8::ff\t64501\tZZ\tB\n"
	for name, line := range map[string]string{
		"no asn":              "192.0.2.0\t192.0.2.255",
		"non-numeric asn":     "192.0.2.0\t192.0.2.255\tAS64500\tZZ\tX",
		"asn above 32 bits":   "192.0.2.0\t192.0.2.255\t4294967296\tZZ\tX",
		"negative asn":        "192.0.2.0\t192.0.2.255\t-1\tZZ\tX",
		"octet above 255":     "192.0.2.0\t192.0.2.256\t64500\tZZ\tX",
		"leading zero octet":  "192.0.2.0\t192.0.2.099\t64500\tZZ\tX",
		"three octets":        "192.0.2\t192.0.2.255\t64500\tZZ\tX",
		"not an address":      "example\t192.0.2.255\t64500\tZZ\tX",
		"reversed IPv4 range": "192.0.2.255\t192.0.2.0\t64500\tZZ\tX",
		"reversed IPv6 range": "2001:db8::ff\t2001:db8::1\t64500\tZZ\tX",
		"mixed families":      "192.0.2.0\t2001:db8::ff\t64500\tZZ\tX",
		"two ellipses":        "2001:db8::1::2\t2001:db8::ff\t64500\tZZ\tX",
		"five-digit group":    "2001:db8::12345\t2001:db8::fffff\t64500\tZZ\tX",
		"nine groups":         "1:2:3:4:5:6:7:8:9\t1:2:3:4:5:6:7:8:a\t64500\tZZ\tX",
		"leading space":       "  192.0.2.0\t192.0.2.255\t64500\tZZ\tX",
		"overlong line":       "192.0.2.0\t192.0.2.255\t64500\tZZ\t" + strings.Repeat("x", 70000),
	} {
		path := filepath.Join(t.TempDir(), "ip2asn.tsv")
		if err := os.WriteFile(path, []byte(valid+line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readDatacenterRanges(path, map[uint32]bool{64500: true}); err == nil {
			t.Errorf("%s: table accepted", name)
		}
	}
}

func TestNewSourceClassifierValidatesConfig(t *testing.T) {
	files := writeSourceFiles(t, testASNTable, testDatacenterASNs)
	for name, cfg := range map[string]SourceClassifierConfig{
		"no table":     {DatacenterASNListPath: files.list},
		"no list":      {ASNDBPath: files.table},
		"bad exempt":   {ASNDBPath: files.table, DatacenterASNListPath: files.list, ExemptCIDRs: []string{"192.0.2.1"}},
		"negative age": {ASNDBPath: files.table, DatacenterASNListPath: files.list, MaxAge: -time.Hour},
	} {
		if _, err := NewSourceClassifier(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// authRefusal decodes an error answer's body.
func authRefusal(t *testing.T, body []byte) brokerapi.ErrorResponse {
	t.Helper()
	var out brokerapi.ErrorResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("error body %q: %v", body, err)
	}
	return out
}

func TestUserAuthRefusesNewIdentitiesFromDatacenterSources(t *testing.T) {
	files := writeSourceFiles(t, testASNTable, testDatacenterASNs)
	store := NewStore()
	h := newUserTestHarness(t, store, Config{
		SourceClassifier: newTestClassifier(t, files, "2001:db8:0:5::/64"),
		EnrollmentBudget: EnrollmentBudget{BurstLimit: 1},
	})

	// Enrolled from a consumer network first.
	existing := newUserKey(t)
	code, enrolled, _ := h.auth(existing)
	if code != http.StatusOK {
		t.Fatalf("consumer enrollment: %d", code)
	}

	for _, source := range []string{"192.0.2.44:4000", "[2001:db8::7]:4000"} {
		h.source = source
		code, _, rec := h.auth(newUserKey(t))
		if code != http.StatusForbidden {
			t.Fatalf("new identity from %s: %d %s", source, code, rec.Body.String())
		}
		if refusal := authRefusal(t, rec.Body.Bytes()); refusal.Code != brokerapi.UserAuthCodeDatacenterSource || refusal.Error == "" {
			t.Fatalf("refusal body = %+v", refusal)
		}
		// The existing identity keeps authenticating from the same network.
		if code, again, _ := h.auth(existing); code != http.StatusOK || again.UserID != enrolled.UserID {
			t.Fatalf("existing identity from %s: %d %+v", source, code, again)
		}
	}
	// No user row and no budget charge for either refusal: the store still
	// holds exactly the one user and only the consumer source was charged.
	store.mu.Lock()
	users, charged := len(store.userIDsByKey), len(store.enrollment)
	store.mu.Unlock()
	if users != 1 || charged != 1 {
		t.Fatalf("store holds %d users and %d charged sources, want 1 and 1", users, charged)
	}
	// An exempt CIDR inside a datacenter range enrolls (once: the budget
	// still applies to it).
	h.source = "[2001:db8:0:5::9]:4000"
	if code, _, rec := h.auth(newUserKey(t)); code != http.StatusOK {
		t.Fatalf("exempt source: %d %s", code, rec.Body.String())
	}
	if code, _, _ := h.auth(newUserKey(t)); code != http.StatusTooManyRequests {
		t.Fatalf("exempt source past its budget: %d, want 429", code)
	}
	if got := h.users.stats.snapshot(); got.enrollDatacenter != 2 || got.enrollUnclassified != 0 {
		t.Fatalf("summary counters = %+v", got)
	}
}

func TestUserAuthAnswers503WithoutClassificationData(t *testing.T) {
	files := writeSourceFiles(t, testASNTable, testDatacenterASNs)
	classifier := newTestClassifier(t, files)
	store := NewStore()
	h := newUserTestHarness(t, store, Config{SourceClassifier: classifier})
	existing := newUserKey(t)
	if code, _, _ := h.auth(existing); code != http.StatusOK {
		t.Fatalf("enrollment with fresh data: %d", code)
	}

	// Data older than MaxAge: a new identity gets a retryable 503 from any
	// network, an existing one still authenticates.
	files.touch(t, h.now.Add(-DefaultSourceDataMaxAge-time.Minute))
	classifier.Reload()
	code, _, rec := h.auth(newUserKey(t))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("stale data: %d %s", code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != strconv.Itoa(int(sourceUnclassifiedRetryAfter/time.Second)) {
		t.Fatalf("Retry-After = %q", got)
	}
	if refusal := authRefusal(t, rec.Body.Bytes()); refusal.Code != brokerapi.UserAuthCodeSourceUnclassified {
		t.Fatalf("503 body = %+v", refusal)
	}
	if code, _, _ := h.auth(existing); code != http.StatusOK {
		t.Fatalf("existing identity with stale data: %d", code)
	}

	// Files gone entirely (after a restart: nothing was ever loaded).
	h.users.sources = newTestClassifier(t, sourceFiles{table: files.table + ".gone", list: files.list})
	if code, _, _ := h.auth(newUserKey(t)); code != http.StatusServiceUnavailable {
		t.Fatalf("missing data: %d, want 503", code)
	}
	store.mu.Lock()
	users := len(store.userIDsByKey)
	store.mu.Unlock()
	if users != 1 {
		t.Fatalf("store holds %d users, want only the first", users)
	}
	if got := h.users.stats.snapshot(); got.enrollUnclassified != 2 {
		t.Fatalf("summary counters = %+v", got)
	}
}

// Without a classifier nothing changes: a datacenter address enrolls.
func TestUserAuthWithoutClassificationIsUnchanged(t *testing.T) {
	h := newUserTestHarness(t, NewStore(), Config{})
	h.source = "192.0.2.44:4000"
	if code, _, rec := h.auth(newUserKey(t)); code != http.StatusOK {
		t.Fatalf("enrollment without classification: %d %s", code, rec.Body.String())
	}
}

func TestHealthzReportsSourceClassification(t *testing.T) {
	files := writeSourceFiles(t, testASNTable, testDatacenterASNs)
	for _, tc := range []struct {
		name       string
		classifier *SourceClassifier
		want       any
	}{
		{"disabled", nil, nil},
		{"available", newTestClassifier(t, files), true},
		{"unavailable", newTestClassifier(t, sourceFiles{table: files.table + ".gone", list: files.list}), false},
	} {
		server := NewServer(NewStore(), Config{SigningSeed: testSigningSeed(), SourceClassifier: tc.classifier})
		req, _ := http.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if got := body["source_classification_available"]; got != tc.want {
			t.Errorf("%s: source_classification_available = %v, want %v", tc.name, got, tc.want)
		}
	}
}
