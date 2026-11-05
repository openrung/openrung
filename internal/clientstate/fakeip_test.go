// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFakeIPBlockRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := NewInDir(dir)
	if _, ok := store.LoadFakeIPBlock(); ok {
		t.Fatal("empty store reported a fake-IP block")
	}
	for _, block := range []int{0, 11} {
		if err := store.SaveFakeIPBlock(block); err != nil {
			t.Fatal(err)
		}
		if got, ok := store.LoadFakeIPBlock(); !ok || got != block {
			t.Fatalf("LoadFakeIPBlock = %d, %v; want %d", got, ok, block)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, fakeIPBlockFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.LoadFakeIPBlock(); ok {
		t.Fatal("corrupt file reported a fake-IP block")
	}
	if !repairableStateFile(fakeIPBlockFile) {
		t.Fatal("a sudo TUN run's fake-IP block file would stay root-owned")
	}
}
