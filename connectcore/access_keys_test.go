// SPDX-License-Identifier: GPL-3.0-or-later

package connectcore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"sync/atomic"
	"testing"
)

// noAccessKey is an installation key that cannot be used, for suites that
// exercise other parts of a host contract requiring a key: the engine logs
// the failure and stays on the public relay list without a network call.
type noAccessKey struct{}

func (noAccessKey) PublicKey() ([]byte, error)  { return nil, errors.New("no key in this test") }
func (noAccessKey) Sign([]byte) ([]byte, error) { return nil, errors.New("no key in this test") }

// memoryKey is an in-memory P-256 installation key that counts signatures.
type memoryKey struct {
	key   *ecdsa.PrivateKey
	signs atomic.Int64
}

func newMemoryKey(t *testing.T) *memoryKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &memoryKey{key: key}
}

func (k *memoryKey) PublicKey() ([]byte, error) { return x509.MarshalPKIXPublicKey(&k.key.PublicKey) }

func (k *memoryKey) Sign(digest []byte) ([]byte, error) {
	k.signs.Add(1)
	r, s, err := ecdsa.Sign(rand.Reader, k.key, digest)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	return raw, nil
}
