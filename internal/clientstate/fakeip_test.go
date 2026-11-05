// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Two Stores over one directory stand in for the desktop app and the CLI:
// every allocation continues from the other's latest record.
func TestAllocateFakeIPBlockSharedAcrossStores(t *testing.T) {
	dir := t.TempDir()
	desktop, cli := NewInDir(dir), NewInDir(dir)
	if desktop.HasFakeIPBlock() {
		t.Fatal("empty store reported a fake-IP block")
	}
	var got []int
	for _, store := range []*Store{desktop, cli, desktop, cli} {
		block, err := store.AllocateFakeIPBlock(16, 14)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, block)
	}
	if want := []int{14, 15, 0, 1}; !equalInts(got, want) {
		t.Fatalf("allocations %v, want %v", got, want)
	}
	if !cli.HasFakeIPBlock() {
		t.Fatal("HasFakeIPBlock false after allocating")
	}
	if !repairableStateFile(fakeIPBlockFile) || !repairableStateFile(fakeIPBlockLockFile) {
		t.Fatal("a sudo TUN run's fake-IP files would stay root-owned")
	}
}

// Concurrent allocations from separate Stores never hand out a block twice.
func TestAllocateFakeIPBlockIsAtomic(t *testing.T) {
	dir := t.TempDir()
	const n = 16
	blocks := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			block, err := NewInDir(dir).AllocateFakeIPBlock(n, 0)
			if err != nil {
				t.Error(err)
				return
			}
			blocks <- block
		}()
	}
	wg.Wait()
	close(blocks)
	seen := map[int]bool{}
	for block := range blocks {
		if seen[block] {
			t.Fatalf("block %d allocated twice", block)
		}
		seen[block] = true
	}
	if len(seen) != n {
		t.Fatalf("allocated %d distinct blocks, want %d", len(seen), n)
	}
}

// An unreadable record leaves the previous block unknown: that allocation
// fails, and the next continues from the record it rewrote.
func TestAllocateFakeIPBlockUnreadableRecord(t *testing.T) {
	dir := t.TempDir()
	store := NewInDir(dir)
	if err := os.WriteFile(filepath.Join(dir, fakeIPBlockFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !store.HasFakeIPBlock() {
		t.Fatal("an unreadable record must count as possibly issued")
	}
	if _, err := store.AllocateFakeIPBlock(16, 3); err == nil {
		t.Fatal("allocated over an unreadable record")
	}
	if block, err := store.AllocateFakeIPBlock(16, 9); err != nil || block != 4 {
		t.Fatalf("next allocation = %d, %v; want 4", block, err)
	}
}

// A record that cannot be written allocates nothing.
func TestAllocateFakeIPBlockWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced on a non-root user")
	}
	dir := t.TempDir()
	store := NewInDir(dir)
	if _, err := store.AllocateFakeIPBlock(16, 2); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if block, err := store.AllocateFakeIPBlock(16, 2); err == nil {
		t.Fatalf("allocated block %d without recording it", block)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
