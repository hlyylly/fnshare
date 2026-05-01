// Package blockstore stores raw shard bytes on the local filesystem.
//
// Layout:
//   <data-dir>/blocks/<aa>/<aa...>/<full-hex>
// where the first 2 hex chars are used as a fan-out directory to avoid
// putting tens of thousands of files in a single directory.
package blockstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Store struct {
	root string
}

func Open(dataDir string) (*Store, error) {
	root := filepath.Join(dataDir, "blocks")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) path(shardID string) string {
	if len(shardID) < 2 {
		return filepath.Join(s.root, shardID)
	}
	return filepath.Join(s.root, shardID[:2], shardID)
}

// Put writes the shard atomically and verifies the content hash matches
// shardID. Returns ErrHashMismatch if not.
func (s *Store) Put(shardID string, data []byte) error {
	if !verifyHash(shardID, data) {
		return ErrHashMismatch
	}
	dst := s.path(shardID)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, dst)
}

func (s *Store) Get(shardID string) ([]byte, error) {
	raw, err := os.ReadFile(s.path(shardID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return raw, err
}

func (s *Store) Has(shardID string) bool {
	_, err := os.Stat(s.path(shardID))
	return err == nil
}

func (s *Store) Delete(shardID string) error {
	err := os.Remove(s.path(shardID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// HashOf returns the canonical shard ID for a payload.
func HashOf(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// HashStream computes SHA-256 of a stream as bytes are read.
func HashStream(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func verifyHash(expectedHex string, data []byte) bool {
	return HashOf(data) == expectedHex
}

var (
	ErrNotFound     = errors.New("blockstore: shard not found")
	ErrHashMismatch = fmt.Errorf("blockstore: payload hash does not match shard id")
)
