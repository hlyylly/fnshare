// Package manifest describes how a file was sharded and where each shard
// lives. M7 introduces multi-stripe layout so files of any size can be
// streamed without loading the whole file into RAM at any layer.
//
// Layout per file:
//
//   plaintext bytes
//     ↓ split into N stripes of StripeDataSize each (last may be shorter)
//   stripe[i] (PlaintextBytes ≤ StripeDataSize)
//     ↓ AES-256-GCM (random nonce per stripe)
//   stripe_payload = nonce(12) || ciphertext
//     ↓ Reed-Solomon (DataShards + ParityShards)
//   shard[i][0..k+m-1]    ← stored on Manifest.Holders[j]
//
// Holders are file-scoped (same k+m peer IDs hold all stripes' shards).
// Each Stripe carries the k+m shard ids for that stripe; the holder list
// is shared at the file level.
package manifest

import (
	"errors"
	"time"

	"github.com/fnshare/fnshare/internal/store"
	"github.com/fxamacker/cbor/v2"
)

type Manifest struct {
	FileID    string `cbor:"fid"  json:"file_id"`
	GroupID   string `cbor:"gid"  json:"group_id"`
	Filename  string `cbor:"name" json:"filename"`
	Size      int64  `cbor:"size" json:"size"` // plaintext bytes

	DataShards     int `cbor:"k"   json:"data_shards"`
	ParityShards   int `cbor:"m"   json:"parity_shards"`
	StripeDataSize int `cbor:"sds" json:"stripe_data_size"` // plaintext bytes per stripe

	Holders []string `cbor:"holders" json:"holders"` // k+m peer IDs; index = shard slot
	Stripes []Stripe `cbor:"stripes" json:"stripes"`

	CreatedAt   time.Time `cbor:"ts"    json:"created_at"`
	OwnerPeerID string    `cbor:"owner" json:"owner_peer_id"`

	// ----- encryption envelope -----
	Mode              string `cbor:"mode" json:"mode"` // "shared" | "private"
	WrappedKey        []byte `cbor:"wk"   json:"-"`
	FilenameEncrypted bool   `cbor:"fne,omitempty" json:"filename_encrypted,omitempty"`
}

// Stripe is one EC group: k+m shards covering one StripeDataSize-byte
// chunk of plaintext (or less, for the last stripe).
type Stripe struct {
	Index           int      `cbor:"i"    json:"index"`
	ShardIDs        []string `cbor:"sids" json:"shard_ids"`        // exactly k+m, parallel to Holders
	PlaintextBytes  int      `cbor:"pb"   json:"plaintext_bytes"`  // ≤ StripeDataSize
	CiphertextBytes int      `cbor:"cb"   json:"ciphertext_bytes"` // = nonce(12) + len(GCM seal)
}

const (
	ModeShared  = "shared"
	ModePrivate = "private"
)

// HolderForShard returns the peer ID that should hold shard slot `idx`.
// Slots run 0..(DataShards+ParityShards-1).
func (m *Manifest) HolderForShard(idx int) string {
	if idx < 0 || idx >= len(m.Holders) {
		return ""
	}
	return m.Holders[idx]
}

const keyPrefix = "manifest/"

func key(fileID string) []byte { return []byte(keyPrefix + fileID) }

func Put(s *store.Store, m *Manifest) error {
	raw, err := cbor.Marshal(m)
	if err != nil {
		return err
	}
	return s.Put(key(m.FileID), raw)
}

func Get(s *store.Store, fileID string) (*Manifest, error) {
	raw, err := s.Get(key(fileID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := cbor.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func List(s *store.Store) ([]*Manifest, error) {
	var out []*Manifest
	err := s.Iterate([]byte(keyPrefix), func(_, v []byte) bool {
		var m Manifest
		if err := cbor.Unmarshal(v, &m); err == nil {
			out = append(out, &m)
		}
		return true
	})
	return out, err
}

func Marshal(m *Manifest) ([]byte, error) { return cbor.Marshal(m) }
func Unmarshal(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := cbor.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

var ErrNotFound = errors.New("manifest: not found")
