// Package ec wraps Reed-Solomon erasure coding for fnshare.
//
// We treat the entire file as a single stripe: the file is padded to a
// multiple of DataShards, split into DataShards equal-sized data shards, and
// ParityShards parity shards are computed. Any DataShards-of-(DataShards
// +ParityShards) shards suffice to reconstruct the file.
//
// This is intentionally simpler than multi-stripe layouts (used by Tahoe-
// LAFS / Storj). For files up to a few hundred MB on a NAS it's plenty;
// multi-stripe is a future M-x optimization for very large files.
package ec

import (
	"errors"
	"fmt"

	"github.com/klauspost/reedsolomon"
)

type Params struct {
	DataShards   int
	ParityShards int
}

// Default for the 3-node testbed: any 2 of 3 shards reconstructs.
// Real deployments with more nodes should use 6+3 / 10+4 etc.
func Default() Params {
	return Params{DataShards: 2, ParityShards: 1}
}

// ShardSize returns the size of each shard for a file of fileSize bytes.
// All shards are the same size; the file is padded with zeroes if needed.
func (p Params) ShardSize(fileSize int64) int {
	if fileSize <= 0 {
		return 0
	}
	d := int64(p.DataShards)
	return int((fileSize + d - 1) / d)
}

// Encode splits data into k+m shards. data is padded with zeroes to a
// multiple of DataShards before encoding.
func (p Params) Encode(data []byte) ([][]byte, error) {
	if p.DataShards <= 0 || p.ParityShards <= 0 {
		return nil, errors.New("ec: invalid params")
	}
	enc, err := reedsolomon.New(p.DataShards, p.ParityShards)
	if err != nil {
		return nil, err
	}
	shards, err := enc.Split(append([]byte(nil), data...))
	if err != nil {
		return nil, fmt.Errorf("split: %w", err)
	}
	if err := enc.Encode(shards); err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	return shards, nil
}

// Decode takes shards (some of which may be nil/missing) and the original
// file size, returning the reconstructed file content.
//
// shards must have len == DataShards + ParityShards. Missing shards should
// be passed as nil; at least DataShards must be present (any positions).
func (p Params) Decode(shards [][]byte, fileSize int64) ([]byte, error) {
	enc, err := reedsolomon.New(p.DataShards, p.ParityShards)
	if err != nil {
		return nil, err
	}
	if len(shards) != p.DataShards+p.ParityShards {
		return nil, fmt.Errorf("ec: expected %d shards, got %d", p.DataShards+p.ParityShards, len(shards))
	}
	if err := enc.Reconstruct(shards); err != nil {
		return nil, fmt.Errorf("reconstruct: %w", err)
	}
	if ok, err := enc.Verify(shards); err != nil || !ok {
		return nil, fmt.Errorf("verify after reconstruct failed: %v", err)
	}

	// Concatenate just the data shards and trim padding.
	out := make([]byte, 0, fileSize)
	for i := 0; i < p.DataShards; i++ {
		out = append(out, shards[i]...)
	}
	if int64(len(out)) > fileSize {
		out = out[:fileSize]
	}
	return out, nil
}
