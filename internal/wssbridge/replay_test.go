package wssbridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryReplayStoreSingleUseBoundAndExpiryCleanup(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	store := NewMemoryReplayStore(1)
	store.now = func() time.Time { return now }
	if consumed, err := store.Consume(context.Background(), "ticket-jti-00000001", now.Add(time.Minute)); err != nil || !consumed {
		t.Fatalf("first consume = %t, %v", consumed, err)
	}
	if consumed, err := store.Consume(context.Background(), "ticket-jti-00000001", now.Add(time.Minute)); err != nil || consumed {
		t.Fatalf("replay consume = %t, %v", consumed, err)
	}
	if _, err := store.Consume(context.Background(), "ticket-jti-00000002", now.Add(time.Minute)); !errors.Is(err, ErrReplayStoreFull) {
		t.Fatalf("full store = %v", err)
	}
	now = now.Add(2 * time.Minute)
	if consumed, err := store.Consume(context.Background(), "ticket-jti-00000002", now.Add(time.Minute)); err != nil || !consumed {
		t.Fatalf("consume after cleanup = %t, %v", consumed, err)
	}
}

func TestDurableReplayStorePersistsAcrossRestartWithoutRawJTI(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "replay.journal")
	jti := "durable-ticket-jti-00000001"

	store, err := OpenDurableReplayStore(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return now }
	if consumed, err := store.Consume(context.Background(), jti, now.Add(time.Minute)); err != nil || !consumed {
		t.Fatalf("first consume = %t, %v", consumed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	journal, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(journal), jti) {
		t.Fatal("durable replay journal contains a raw JTI")
	}

	restarted, err := OpenDurableReplayStore(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.now = func() time.Time { return now }
	if consumed, err := restarted.Consume(context.Background(), jti, now.Add(time.Minute)); err != nil || consumed {
		t.Fatalf("post-restart replay = %t, %v", consumed, err)
	}
	now = now.Add(2 * time.Minute)
	if consumed, err := restarted.Consume(context.Background(), "durable-ticket-jti-00000002", now.Add(time.Minute)); err != nil || !consumed {
		t.Fatalf("consume after durable prune = %t, %v", consumed, err)
	}
}

func TestDurableReplayStoreConsumeIsAtomicAndExclusivelyLocked(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "replay.journal")
	store, err := OpenDurableReplayStore(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.now = func() time.Time { return now }
	if duplicate, err := OpenDurableReplayStore(path, 32); err == nil {
		_ = duplicate.Close()
		t.Fatal("second process lock was accepted")
	}

	var consumed atomic.Int64
	var failures atomic.Int64
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			ok, err := store.Consume(context.Background(), "concurrent-ticket-jti-000001", now.Add(time.Minute))
			if err != nil {
				failures.Add(1)
			}
			if ok {
				consumed.Add(1)
			}
		}()
	}
	wait.Wait()
	if consumed.Load() != 1 || failures.Load() != 0 {
		t.Fatalf("atomic consumes = %d, failures = %d", consumed.Load(), failures.Load())
	}
}

func TestDurableReplayStoreRepairsCrashTailAndRejectsUnsafeState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "replay.journal")
	store, err := OpenDurableReplayStore(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.WriteString("unterminated-crash-tail"); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	repaired, err := OpenDurableReplayStore(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := repaired.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != replayJournalHeader {
		t.Fatalf("repaired journal = %q", data)
	}

	if _, err := OpenDurableReplayStore("relative/replay.journal", 4); err == nil {
		t.Fatal("relative replay-state path was accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDurableReplayStore(path, 4); err == nil {
		t.Fatal("group/world-readable replay state was accepted")
	}
}

func TestDurableReplayStorePrunesAndCompactsWithinBound(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "replay.journal")
	store, err := OpenDurableReplayStore(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.now = func() time.Time { return now }
	for index := range minCompactStaleRecords + 16 {
		jti := "compaction-ticket-jti-" + strings.Repeat("0", 8-len(strconv.Itoa(index))) + strconv.Itoa(index)
		if consumed, err := store.Consume(context.Background(), jti, now.Add(time.Second)); err != nil || !consumed {
			t.Fatalf("consume %d = %t, %v", index, consumed, err)
		}
		now = now.Add(2 * time.Second)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size() >= int64(32*replayRecordMaxBytes) {
		t.Fatalf("expired replay journal was not compacted: %d bytes", stat.Size())
	}
}
