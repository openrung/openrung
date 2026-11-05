// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

const (
	fakeIPBlockFile     = "fakeip-block.json"
	fakeIPBlockLockFile = "fakeip-block.lock"
)

// storedFakeIPBlock is the fake-IP block of the most recent tunnel run. It
// holds only the block number: the address-to-hostname table never leaves
// sing-box's memory.
type storedFakeIPBlock struct {
	Block int `json:"block"`
}

// AllocateFakeIPBlock records and returns the block for a new tunnel run:
// the one after the recorded block, or initial when none is recorded. The
// read and the write happen under a file lock, so the desktop app and the
// CLI, which share this directory, never hand consecutive runs the same
// block. An unreadable record is replaced by initial but reported as an
// error, so the run that finds it hands out no fake addresses: the block
// before it is unknown.
func (s *Store) AllocateFakeIPBlock(count, initial int) (int, error) {
	if count <= 0 || initial < 0 || initial >= count {
		return 0, fmt.Errorf("fake-IP block %d is outside [0, %d)", initial, count)
	}
	var block int
	err := withFileLock(filepath.Join(s.dir, fakeIPBlockLockFile), func() error {
		next := initial
		var unreadable error
		data, err := s.readFile(fakeIPBlockFile)
		switch {
		case err == nil:
			var stored storedFakeIPBlock
			if json.Unmarshal(data, &stored) != nil || stored.Block < 0 || stored.Block >= count {
				unreadable = errors.New("the recorded fake-IP block is unreadable")
			} else {
				next = (stored.Block + 1) % count
			}
		case errors.Is(err, fs.ErrNotExist):
		default:
			return err
		}
		if err := s.writeJSON(fakeIPBlockFile, storedFakeIPBlock{Block: next}); err != nil {
			return err
		}
		if unreadable != nil {
			return unreadable
		}
		block = next
		return nil
	})
	return block, err
}

// HasFakeIPBlock reports whether a block may ever have been recorded: true
// unless the record is known to be absent.
func (s *Store) HasFakeIPBlock() bool {
	_, err := s.readFile(fakeIPBlockFile)
	return !errors.Is(err, fs.ErrNotExist)
}
