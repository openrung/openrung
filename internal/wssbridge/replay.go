package wssbridge

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrReplayStoreFull   = errors.New("WSS bridge replay store is full")
	ErrReplayStoreClosed = errors.New("WSS bridge replay store is closed")
)

const (
	replayJournalHeader    = "openrung-wss-replay-v1\n"
	replayRecordMaxBytes   = 96
	defaultReplayEntries   = 100_000
	minCompactStaleRecords = 128
	MaxReplayEntries       = 1_000_000
)

type ReplayStore interface {
	Consume(ctx context.Context, jti string, expiresAt time.Time) (consumed bool, err error)
}

// replayKey keeps raw ticket identifiers out of both persistent state and
// diagnostic artifacts. Equality of the full SHA-256 value is sufficient for
// replay detection; a collision can only fail closed by rejecting a ticket.
type replayKey [sha256.Size]byte

func newReplayKey(jti string) replayKey { return sha256.Sum256([]byte(jti)) }

type replayExpiry struct {
	key       replayKey
	expiresAt int64
}

type replayExpiryHeap []replayExpiry

func (h replayExpiryHeap) Len() int           { return len(h) }
func (h replayExpiryHeap) Less(i, j int) bool { return h[i].expiresAt < h[j].expiresAt }
func (h replayExpiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *replayExpiryHeap) Push(value any)    { *h = append(*h, value.(replayExpiry)) }
func (h *replayExpiryHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

type replayIndex struct {
	entries map[replayKey]int64
	expiry  replayExpiryHeap
}

func newReplayIndex() replayIndex {
	return replayIndex{entries: make(map[replayKey]int64)}
}

func (i *replayIndex) contains(key replayKey, nowUnixNano int64) bool {
	expiresAt, exists := i.entries[key]
	return exists && expiresAt > nowUnixNano
}

func (i *replayIndex) insert(key replayKey, expiresAt int64) {
	i.entries[key] = expiresAt
	heap.Push(&i.expiry, replayExpiry{key: key, expiresAt: expiresAt})
}

func (i *replayIndex) prune(nowUnixNano int64) int {
	removed := 0
	for i.expiry.Len() > 0 && i.expiry[0].expiresAt <= nowUnixNano {
		candidate := heap.Pop(&i.expiry).(replayExpiry)
		if current, exists := i.entries[candidate.key]; exists && current == candidate.expiresAt {
			delete(i.entries, candidate.key)
			removed++
		}
	}
	return removed
}

// MemoryReplayStore is a bounded single-process replay store. It is useful for
// tests and embedded callers that explicitly accept process-local durability.
// The production sidecar command uses DurableReplayStore instead.
type MemoryReplayStore struct {
	mu         sync.Mutex
	index      replayIndex
	maxEntries int
	now        func() time.Time
}

func NewMemoryReplayStore(maxEntries int) *MemoryReplayStore {
	if maxEntries <= 0 {
		maxEntries = defaultReplayEntries
	}
	return &MemoryReplayStore{index: newReplayIndex(), maxEntries: maxEntries, now: time.Now}
}

func (s *MemoryReplayStore) Consume(ctx context.Context, jti string, expiresAt time.Time) (bool, error) {
	if err := validateReplayConsume(ctx, jti, expiresAt); err != nil {
		return false, err
	}
	if s == nil || s.maxEntries <= 0 || s.now == nil {
		return false, errors.New("replay store is not initialized")
	}
	nowUnixNano := s.now().UTC().UnixNano()
	if expiresAt.UTC().UnixNano() <= nowUnixNano {
		return false, ErrExpiredTicket
	}
	key := newReplayKey(jti)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.index.prune(nowUnixNano)
	if s.index.contains(key, nowUnixNano) {
		return false, nil
	}
	if len(s.index.entries) >= s.maxEntries {
		return false, ErrReplayStoreFull
	}
	s.index.insert(key, expiresAt.UTC().UnixNano())
	return true, nil
}

// DurableReplayStore is a bounded, relay-local append-and-fsync replay
// journal. Consume returns true only after the hashed JTI is durable. A
// separate lifetime lock prevents two sidecar processes from sharing the same
// state path, while atomic compaction preserves that lock across file renames.
//
// The journal contains only SHA-256 hashes and expiration timestamps. It never
// stores bearer tickets, payload bytes, relay/front labels, or viewer addresses.
type DurableReplayStore struct {
	mu             sync.Mutex
	path           string
	lock           *os.File
	journal        *os.File
	index          replayIndex
	maxEntries     int
	journalRecords int
	now            func() time.Time
	closed         bool
	broken         bool
}

// OpenDurableReplayStore opens or creates a versioned replay journal. The
// parent directory must already exist, and the state path must be absolute.
// Existing state and its lock must be regular owner-only files.
func OpenDurableReplayStore(path string, maxEntries int) (_ *DurableReplayStore, err error) {
	path, err = normalizeReplayStatePath(path)
	if err != nil {
		return nil, err
	}
	if maxEntries <= 0 {
		maxEntries = defaultReplayEntries
	}
	if maxEntries > MaxReplayEntries {
		return nil, fmt.Errorf("replay entry limit must not exceed %d", MaxReplayEntries)
	}
	lock, err := openSecureReplayFile(path+".lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, fmt.Errorf("open replay-state lock: %w", err)
	}
	locked := false
	defer func() {
		if err == nil {
			return
		}
		if locked {
			_ = unlockReplayFile(lock)
		}
		_ = lock.Close()
	}()
	if err = lockReplayFile(lock); err != nil {
		return nil, fmt.Errorf("lock replay state: %w", err)
	}
	locked = true

	store := &DurableReplayStore{
		path: path, lock: lock, index: newReplayIndex(), maxEntries: maxEntries, now: time.Now,
	}
	if err = store.load(); err != nil {
		if store.journal != nil {
			_ = store.journal.Close()
			store.journal = nil
		}
		return nil, err
	}
	return store, nil
}

func (s *DurableReplayStore) Consume(ctx context.Context, jti string, expiresAt time.Time) (bool, error) {
	if err := validateReplayConsume(ctx, jti, expiresAt); err != nil {
		return false, err
	}
	if s == nil {
		return false, errors.New("replay store is not initialized")
	}
	key := newReplayKey(jti)
	expiresUnixNano := expiresAt.UTC().UnixNano()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.broken || s.journal == nil || s.now == nil {
		return false, ErrReplayStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	nowUnixNano := s.now().UTC().UnixNano()
	if expiresUnixNano <= nowUnixNano {
		return false, ErrExpiredTicket
	}
	s.index.prune(nowUnixNano)
	if s.index.contains(key, nowUnixNano) {
		return false, nil
	}
	if len(s.index.entries) >= s.maxEntries {
		return false, ErrReplayStoreFull
	}
	if s.shouldCompact() {
		if err := s.compactLocked(); err != nil {
			return false, fmt.Errorf("compact replay state: %w", err)
		}
	}
	record := encodeReplayRecord(key, expiresUnixNano)
	if _, err := s.journal.Write(record); err != nil {
		s.broken = true
		return false, fmt.Errorf("append replay state: %w", err)
	}
	if err := s.journal.Sync(); err != nil {
		s.broken = true
		return false, fmt.Errorf("sync replay state: %w", err)
	}
	s.index.insert(key, expiresUnixNano)
	s.journalRecords++
	return true, nil
}

// Close flushes the journal and releases its exclusive process lock.
func (s *DurableReplayStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	if s.journal != nil {
		if err := s.journal.Sync(); err != nil {
			errs = append(errs, err)
		}
		if err := s.journal.Close(); err != nil {
			errs = append(errs, err)
		}
		s.journal = nil
	}
	if s.lock != nil {
		if err := unlockReplayFile(s.lock); err != nil {
			errs = append(errs, err)
		}
		if err := s.lock.Close(); err != nil {
			errs = append(errs, err)
		}
		s.lock = nil
	}
	return errors.Join(errs...)
}

func (s *DurableReplayStore) load() error {
	journal, err := openSecureReplayFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND)
	if err != nil {
		return fmt.Errorf("open replay state: %w", err)
	}
	s.journal = journal
	failed := true
	defer func() {
		if failed {
			_ = journal.Close()
			s.journal = nil
		}
	}()
	stat, err := journal.Stat()
	if err != nil {
		return fmt.Errorf("stat replay state: %w", err)
	}
	if stat.Size() > maxReplayJournalBytes(s.maxEntries) {
		return errors.New("replay state exceeds its bounded size")
	}
	if _, err := journal.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek replay state: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(journal, maxReplayJournalBytes(s.maxEntries)+1))
	if err != nil {
		return fmt.Errorf("read replay state: %w", err)
	}
	nowUnixNano := s.now().UTC().UnixNano()
	needsCompact := false
	if len(data) == 0 {
		needsCompact = true
	} else {
		if !bytes.HasPrefix(data, []byte(replayJournalHeader)) {
			return errors.New("replay state has an unsupported or corrupt header")
		}
		records := data[len(replayJournalHeader):]
		if len(records) > 0 && records[len(records)-1] != '\n' {
			// Consume never returns success before a complete record is synced.
			// An unterminated crash tail therefore represents no granted ticket.
			if cut := bytes.LastIndexByte(records, '\n'); cut >= 0 {
				records = records[:cut+1]
			} else {
				records = nil
			}
			needsCompact = true
		}
		scanner := bufio.NewScanner(bytes.NewReader(records))
		scanner.Buffer(make([]byte, replayRecordMaxBytes), replayRecordMaxBytes)
		for scanner.Scan() {
			key, expiresAt, parseErr := parseReplayRecord(scanner.Text())
			if parseErr != nil {
				return parseErr
			}
			s.journalRecords++
			if expiresAt <= nowUnixNano {
				needsCompact = true
				continue
			}
			if existing, duplicate := s.index.entries[key]; duplicate {
				needsCompact = true
				if existing >= expiresAt {
					continue
				}
			}
			s.index.insert(key, expiresAt)
		}
		if err := scanner.Err(); err != nil {
			return errors.New("replay state contains an oversized record")
		}
	}
	if len(s.index.entries) > s.maxEntries {
		return ErrReplayStoreFull
	}
	if needsCompact || s.shouldCompact() {
		if err := s.compactLocked(); err != nil {
			return fmt.Errorf("initialize replay state: %w", err)
		}
	}
	failed = false
	return nil
}

func (s *DurableReplayStore) shouldCompact() bool {
	stale := s.journalRecords - len(s.index.entries)
	threshold := s.maxEntries / 2
	if threshold < minCompactStaleRecords {
		threshold = minCompactStaleRecords
	}
	return stale >= threshold
}

func (s *DurableReplayStore) compactLocked() error {
	directory, base := filepath.Dir(s.path), filepath.Base(s.path)
	temporary, err := os.CreateTemp(directory, "."+base+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	committed := false
	defer func() {
		_ = temporary.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := io.WriteString(temporary, replayJournalHeader); err != nil {
		return err
	}
	for key, expiresAt := range s.index.entries {
		if _, err := temporary.Write(encodeReplayRecord(key, expiresAt)); err != nil {
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, s.path); err != nil {
		return err
	}
	committed = true
	replacement, err := openSecureReplayFile(s.path, os.O_RDWR|os.O_APPEND)
	if err != nil {
		s.broken = true
		return err
	}
	previous := s.journal
	s.journal = replacement
	s.journalRecords = len(s.index.entries)
	if previous != nil {
		if err := previous.Close(); err != nil {
			s.broken = true
			return err
		}
	}
	if err := syncDirectory(directory); err != nil {
		s.broken = true
		return err
	}
	return nil
}

func normalizeReplayStatePath(path string) (string, error) {
	if path == "" || strings.TrimSpace(path) != path || !filepath.IsAbs(path) {
		return "", errors.New("replay-state path must be an absolute path without surrounding whitespace")
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) || clean != path {
		return "", errors.New("replay-state path must be clean and name a file")
	}
	return clean, nil
}

func openSecureReplayFile(path string, flags int) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("replay-state path must name a regular file")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("replay-state file permissions must be owner-only")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = file.Close()
		return nil, errors.New("replay-state file must be regular with owner-only permissions")
	}
	return file, nil
}

func validateReplayConsume(ctx context.Context, jti string, expiresAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validID(jti, 16, 128) {
		return errors.New("replay JTI is invalid")
	}
	if expiresAt.IsZero() {
		return ErrExpiredTicket
	}
	return nil
}

func encodeReplayRecord(key replayKey, expiresAt int64) []byte {
	encoded := base64.RawURLEncoding.EncodeToString(key[:])
	return []byte(encoded + " " + strconv.FormatInt(expiresAt, 10) + "\n")
}

func parseReplayRecord(line string) (replayKey, int64, error) {
	var key replayKey
	parts := strings.Split(line, " ")
	if len(parts) != 2 {
		return key, 0, errors.New("replay state contains a malformed record")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(raw) != sha256.Size || base64.RawURLEncoding.EncodeToString(raw) != parts[0] {
		return key, 0, errors.New("replay state contains a malformed identifier hash")
	}
	expiresAt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || expiresAt <= 0 || strconv.FormatInt(expiresAt, 10) != parts[1] {
		return key, 0, errors.New("replay state contains a malformed expiration")
	}
	copy(key[:], raw)
	return key, expiresAt, nil
}

func maxReplayJournalBytes(maxEntries int) int64 {
	threshold := maxEntries / 2
	if threshold < minCompactStaleRecords {
		threshold = minCompactStaleRecords
	}
	return int64(len(replayJournalHeader) + (maxEntries+threshold+1)*replayRecordMaxBytes)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
