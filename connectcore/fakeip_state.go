// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"encoding/json"
	"errors"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"

	"github.com/openrung/openrung/connectcore/client"
)

// fakeIPRunState is the record kept beside the fake-IP cache: the address
// block the latest run answered from, and whether that run closed the cache
// cleanly. It holds no hostname.
type fakeIPRunState struct {
	Block int  `json:"block"`
	Clean bool `json:"clean"`
}

func fakeIPRunStatePath(cachePath string) string { return cachePath + ".run" }

// beginFakeIPRun picks the block for a run that is about to open the cache at
// cachePath and records it, durably, as not yet closed. After a clean close
// the block and the cache stay, so the cached mappings stay valid. After any
// other end — a crash, a forced kill, power loss, a run that never let go of
// the cache — sing-box may not have saved its allocation position and would
// hand out again, for other hostnames, addresses apps still hold. So the run
// moves to the next block and starts from an empty cache: the stale addresses
// are then outside its table and refused. The cache file is deleted here
// rather than left to sing-box's own reset on a range change, which does
// nothing when the cache lacks one of its fake-IP buckets (no AAAA answer
// was ever stored, for instance) and would keep answering from the old block.
// An error means nothing was recorded; the run must not use the cache.
func beginFakeIPRun(cachePath string) (int, error) {
	prev, err := readFakeIPRunState(cachePath)
	if err == nil && prev.Clean {
		return prev.Block, writeFakeIPRunState(cachePath, fakeIPRunState{Block: prev.Block})
	}
	// No record means no run is known to have used this cache; an unreadable
	// one (writes are atomic, so only outside interference) leaves the
	// previous block unknown. Either way a random block is the best choice.
	block := rand.IntN(client.FakeIPBlockCount)
	if err == nil {
		block = (prev.Block + 1) % client.FakeIPBlockCount
	}
	if err := os.Remove(cachePath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if err := writeFakeIPRunState(cachePath, fakeIPRunState{Block: block}); err != nil {
		return 0, err
	}
	return block, nil
}

// endFakeIPRun records that the run on block closed the cache cleanly.
func endFakeIPRun(cachePath string, block int) error {
	return writeFakeIPRunState(cachePath, fakeIPRunState{Block: block, Clean: true})
}

func readFakeIPRunState(cachePath string) (fakeIPRunState, error) {
	var state fakeIPRunState
	data, err := os.ReadFile(fakeIPRunStatePath(cachePath))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.Block < 0 || state.Block >= client.FakeIPBlockCount {
		return state, errors.New("fake-IP run state names no valid block")
	}
	return state, nil
}

// writeFakeIPRunState replaces the record atomically and durably: the "not
// yet closed" record must survive the very power loss it guards against.
func writeFakeIPRunState(cachePath string, state fakeIPRunState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := fakeIPRunStatePath(cachePath)
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		// The rename itself must reach the disk too.
		if d, err := os.Open(dir); err == nil {
			err = d.Sync()
			_ = d.Close()
			if err != nil {
				return err
			}
		}
	}
	return nil
}
