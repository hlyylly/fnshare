// Package ledger tracks per-peer storage and bandwidth accounting.
//
// M3 scope: LOCAL view only. Each node records what it has done with each
// peer (bytes we stored for them, bytes they downloaded from us, etc.).
// Cross-node gossip / signed entries / global ranking is M3.5+.
//
// Two complementary numbers per peer:
//   StoredForThemBytes  — they pushed shards to us; this is their cost
//                         to us as a "storage host".
//   ServedToThemBytes   — they downloaded shards from us; this is what
//                         they consumed (from our perspective).
//
// In a healthy circle the two should roughly balance over time. Big skews
// surface "bandwidth leeches" without needing a real economy.
package ledger

import (
	"errors"
	"sync"
	"time"

	"github.com/fnshare/fnshare/internal/store"
	"github.com/fxamacker/cbor/v2"
)

// Entry tracks accounting + liveness state for one remote peer.
//
// Traffic counters (4 directions):
//   StoredForThemBytes    — they pushed a shard to us
//   ServedToThemBytes     — they pulled a shard from us
//   StoredOnThemBytes     — we pushed a shard to them
//   DownloadedFromBytes   — we pulled a shard from them
//
// Liveness (M5):
//   IsOnline       — set false after ConsecFailures hits the heartbeat threshold
//   ConsecFailures — current streak of failed pings, reset on success
//   LastSeenAt     — last successful ping
//   OfflineSince   — when we last marked them offline (zero if online)
//   Reputation     — heuristic 0..max, ticks down with consecutive failures
//                    and back up with successes. New peers start at ReputationBaseline.
type Entry struct {
	PeerID              string    `cbor:"peer"     json:"peer_id"`
	StoredForThemBytes  int64     `cbor:"stored"   json:"stored_for_them_bytes"`
	ServedToThemBytes   int64     `cbor:"served"   json:"served_to_them_bytes"`
	StoredOnThemBytes   int64     `cbor:"on"       json:"stored_on_them_bytes"`
	DownloadedFromBytes int64     `cbor:"dl"       json:"downloaded_from_bytes"`

	IsOnline       bool      `cbor:"online"   json:"is_online"`
	ConsecFailures int       `cbor:"cf"       json:"consec_failures"`
	LastPingAt     time.Time `cbor:"pat"      json:"last_ping_at,omitempty"`
	LastSeenAt     time.Time `cbor:"sat"      json:"last_seen_at,omitempty"`
	OfflineSince   time.Time `cbor:"osince"   json:"offline_since,omitempty"`
	Reputation     int64     `cbor:"rep"      json:"reputation"`

	UpdatedAt time.Time `cbor:"ts"       json:"updated_at"`
}

// Heartbeat parameters. Conservative defaults for production. Tests can
// override via env (FNSHARE_PING_INTERVAL etc) — wired in main.go.
const (
	OfflineThreshold      = 3   // consecutive failures before offline
	ReputationBaseline    = 100 // new peers start here
	ReputationMin         = 0
	ReputationMax         = 100
	ReputationGainOnPing  = 1
	ReputationLossOnFail  = 5 // per consecutive failure beyond threshold
)

const keyPrefix = "ledger/local/"

func key(peerID string) []byte { return []byte(keyPrefix + peerID) }

// Ledger is a thin write-coalescing cache around the persistent ledger.
// Block-protocol handlers call RecordStored / RecordServed on every byte
// of traffic, so we keep the hot path lock-cheap and Flush periodically.
type Ledger struct {
	store *store.Store

	mu      sync.Mutex
	dirty   map[string]*Entry
}

func New(s *store.Store) *Ledger {
	return &Ledger{store: s, dirty: map[string]*Entry{}}
}

// RecordStoredForThem: peer pushed `n` bytes to us (we stored them on disk).
func (l *Ledger) RecordStoredForThem(peerID string, n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touchLocked(peerID).StoredForThemBytes += n
}

// RecordServedToThem: we sent `n` bytes to peer in response to their GET.
func (l *Ledger) RecordServedToThem(peerID string, n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touchLocked(peerID).ServedToThemBytes += n
}

// RecordStoredOnThem: we pushed `n` bytes to peer (they're holding for us).
func (l *Ledger) RecordStoredOnThem(peerID string, n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touchLocked(peerID).StoredOnThemBytes += n
}

// RecordDownloadedFrom: we pulled `n` bytes from peer (we consumed cycles).
func (l *Ledger) RecordDownloadedFrom(peerID string, n int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.touchLocked(peerID).DownloadedFromBytes += n
}

// PingResult is what RecordPing returns so the caller can react to state
// transitions (e.g., trigger a repair scan when a peer just went offline).
type PingResult struct {
	WentOffline bool // peer was online, now isn't
	WentOnline  bool // peer was offline, now is
	IsOnline    bool // current state
	Reputation  int64
}

// RecordPing updates a peer's liveness + reputation based on a probe result.
// First-ever observation initializes Reputation to ReputationBaseline and
// sets IsOnline based on the probe.
func (l *Ledger) RecordPing(peerID string, ok bool) PingResult {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.touchLocked(peerID)
	wasOnline := e.IsOnline
	// Brand-new entry detection: zero LastPingAt means we haven't probed yet.
	firstProbe := e.LastPingAt.IsZero()
	if firstProbe {
		e.Reputation = ReputationBaseline
		// First probe: treat success as online, failure as still-undecided
		// (don't immediately punish a brand-new peer for one missed packet).
		wasOnline = ok
	}
	e.LastPingAt = time.Now().UTC()

	if ok {
		e.ConsecFailures = 0
		e.LastSeenAt = e.LastPingAt
		e.IsOnline = true
		if !e.OfflineSince.IsZero() {
			e.OfflineSince = time.Time{}
		}
		if e.Reputation < ReputationMax {
			e.Reputation += ReputationGainOnPing
			if e.Reputation > ReputationMax {
				e.Reputation = ReputationMax
			}
		}
	} else {
		e.ConsecFailures++
		if e.ConsecFailures >= OfflineThreshold {
			e.IsOnline = false
			if e.OfflineSince.IsZero() {
				e.OfflineSince = e.LastPingAt
			}
			// Reputation penalty grows with the streak — light at first
			// (transient blip), heavier the longer they're gone.
			loss := int64(ReputationLossOnFail)
			if e.Reputation-loss < ReputationMin {
				loss = e.Reputation - ReputationMin
			}
			if loss > 0 {
				e.Reputation -= loss
			}
		}
	}

	return PingResult{
		WentOffline: wasOnline && !e.IsOnline,
		WentOnline:  !wasOnline && e.IsOnline,
		IsOnline:    e.IsOnline,
		Reputation:  e.Reputation,
	}
}

// IsOnline returns the current liveness flag for a peer (false for unknown).
func (l *Ledger) IsOnline(peerID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.dirty[peerID]; ok {
		return e.IsOnline
	}
	if e := loadEntry(l.store, peerID); e != nil {
		return e.IsOnline
	}
	return false
}

func (l *Ledger) touchLocked(peerID string) *Entry {
	if e, ok := l.dirty[peerID]; ok {
		e.UpdatedAt = time.Now().UTC()
		return e
	}
	// Pull from disk so subsequent flushes are additive, not destructive.
	e := loadEntry(l.store, peerID)
	if e == nil {
		e = &Entry{PeerID: peerID}
	}
	e.UpdatedAt = time.Now().UTC()
	l.dirty[peerID] = e
	return e
}

// Flush writes pending updates to disk. Safe to call from any goroutine.
func (l *Ledger) Flush() error {
	l.mu.Lock()
	pending := l.dirty
	l.dirty = map[string]*Entry{}
	l.mu.Unlock()

	for _, e := range pending {
		raw, err := cbor.Marshal(e)
		if err != nil {
			return err
		}
		if err := l.store.Put(key(e.PeerID), raw); err != nil {
			return err
		}
	}
	return nil
}

// All returns the merged on-disk + dirty view.
func (l *Ledger) All() ([]*Entry, error) {
	disk := map[string]*Entry{}
	err := l.store.Iterate([]byte(keyPrefix), func(_, v []byte) bool {
		var e Entry
		if err := cbor.Unmarshal(v, &e); err == nil {
			disk[e.PeerID] = &e
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	for pid, e := range l.dirty {
		disk[pid] = e
	}
	l.mu.Unlock()

	out := make([]*Entry, 0, len(disk))
	for _, e := range disk {
		out = append(out, e)
	}
	return out, nil
}

func loadEntry(s *store.Store, peerID string) *Entry {
	raw, err := s.Get(key(peerID))
	if errors.Is(err, store.ErrNotFound) || err != nil {
		return nil
	}
	var e Entry
	if cbor.Unmarshal(raw, &e) != nil {
		return nil
	}
	return &e
}
