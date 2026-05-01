// Package spool stages shards (and manifests) destined for currently-
// unreachable holders, on the local filesystem, so that uploads don't
// block when any one friend's NAS happens to be offline.
//
// The Worker (worker.go) periodically attempts to deliver spooled items
// to peers the heartbeat layer has marked back online.
//
// Layout:
//
//   <data>/spool/<peer_id>/s_<shard_id>     ← raw shard bytes
//   <data>/spool/<peer_id>/m_<file_id>      ← raw manifest CBOR bytes
//
// Each entry is at most a few MiB (a single EC stripe shard); a full disk
// is the only reason we'd refuse to spool — in that case the upload fails
// just like before.
package spool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Spool struct {
	root string

	mu sync.Mutex
}

// Kind distinguishes shard payloads from manifest payloads in the spool
// since they go through different network operations on flush.
type Kind string

const (
	KindShard    Kind = "s"
	KindManifest Kind = "m"
)

// Entry describes one queued item. The on-disk path is internal; callers
// use Read/Remove with the entry handle.
type Entry struct {
	PeerID string
	Kind   Kind
	Key    string // shard id or file id
	path   string
}

func Open(dataDir string) (*Spool, error) {
	root := filepath.Join(dataDir, "spool")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Spool{root: root}, nil
}

// EnqueueShard stages a shard for delivery to peerID.
func (s *Spool) EnqueueShard(peerID, shardID string, data []byte) error {
	return s.write(peerID, KindShard, shardID, data)
}

// EnqueueManifest stages a manifest CBOR blob for delivery to peerID.
func (s *Spool) EnqueueManifest(peerID, fileID string, manifestRaw []byte) error {
	return s.write(peerID, KindManifest, fileID, manifestRaw)
}

func (s *Spool) write(peerID string, kind Kind, key string, data []byte) error {
	if peerID == "" || key == "" {
		return errors.New("spool: empty peer id or key")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.root, peerID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dir, string(kind)+"_"+key)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
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

// PendingPeers returns the peer IDs that currently have queued items.
func (s *Spool) PendingPeers() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dirs, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d.IsDir() {
			out = append(out, d.Name())
		}
	}
	return out, nil
}

// ListEntries returns every queued item for one peer. Order is filesystem-
// determined and not load-bearing — callers should be tolerant.
func (s *Spool) ListEntries(peerID string) ([]Entry, error) {
	dir := filepath.Join(s.root, peerID)
	files, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Entry, 0, len(files))
	for _, f := range files {
		name := f.Name()
		if strings.HasPrefix(name, ".tmp-") {
			continue
		}
		// name = "<kind>_<key>"
		if len(name) < 3 || name[1] != '_' {
			continue
		}
		kind := Kind(name[:1])
		if kind != KindShard && kind != KindManifest {
			continue
		}
		out = append(out, Entry{
			PeerID: peerID,
			Kind:   kind,
			Key:    name[2:],
			path:   filepath.Join(dir, name),
		})
	}
	return out, nil
}

// Read returns the bytes for an entry.
func (s *Spool) Read(e Entry) ([]byte, error) {
	if e.path == "" {
		return nil, errors.New("spool: empty entry path")
	}
	return os.ReadFile(e.path)
}

// Remove deletes one entry from the spool. Idempotent.
func (s *Spool) Remove(e Entry) error {
	if e.path == "" {
		return nil
	}
	if err := os.Remove(e.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Best-effort: prune the peer directory if now empty.
	_ = os.Remove(filepath.Dir(e.path))
	return nil
}

// PendingForPeer is a convenience that returns the count of entries queued
// for a peer (for status display).
func (s *Spool) PendingForPeer(peerID string) int {
	entries, _ := s.ListEntries(peerID)
	return len(entries)
}

// Stats returns total queued bytes across all peers (for UI display).
func (s *Spool) Stats() (peers int, totalBytes int64) {
	dirs, err := os.ReadDir(s.root)
	if err != nil {
		return 0, 0
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		peers++
		entries, _ := s.ListEntries(d.Name())
		for _, e := range entries {
			info, err := os.Stat(e.path)
			if err == nil {
				totalBytes += info.Size()
			}
		}
	}
	return
}

// debug helper: format an entry for log lines.
func (e Entry) String() string {
	return fmt.Sprintf("%s/%s/%s", e.PeerID, e.Kind, shortKey(e.Key))
}

func shortKey(k string) string {
	if len(k) <= 12 {
		return k
	}
	return k[:12]
}
