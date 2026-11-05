// SPDX-License-Identifier: GPL-3.0-or-later

package clientstate

import "encoding/json"

const fakeIPBlockFile = "fakeip-block.json"

// storedFakeIPBlock is the fake-IP block of the most recent tunnel run. It
// holds only the block number: the address-to-hostname table never leaves
// sing-box's memory.
type storedFakeIPBlock struct {
	Block int `json:"block"`
}

// LoadFakeIPBlock returns the stored block and whether one was stored; an
// unreadable or corrupt file counts as none.
func (s *Store) LoadFakeIPBlock() (int, bool) {
	data, err := s.readFile(fakeIPBlockFile)
	if err != nil {
		return 0, false
	}
	var stored storedFakeIPBlock
	if err := json.Unmarshal(data, &stored); err != nil {
		return 0, false
	}
	return stored.Block, true
}

// SaveFakeIPBlock records the block a tunnel run is about to answer from.
func (s *Store) SaveFakeIPBlock(block int) error {
	return s.writeJSON(fakeIPBlockFile, storedFakeIPBlock{Block: block})
}
