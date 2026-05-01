// Package holders picks which group members store a given file's shards.
//
// The previous (M2-M6) strategy was "sort peer ids alphabetically, take
// first k+m" — deterministic but pathological: every file landed on the
// SAME k+m members, leaving the rest of the group's contributed space
// permanently unused.
//
// M7 uses Highest-Random-Weight (HRW / "rendezvous") hashing: for each
// file id, compute hash(peer || file_id) for every member and take the
// top k+m by score. Properties:
//
//   - For a given file, all nodes independently agree on the holder set
//     (no coordination needed).
//   - Across many files, the load spreads ~uniformly over N members.
//   - When a member joins or leaves, only ~(k+m)/N of files need to
//     migrate (good for repair load).
package holders

import (
	"crypto/sha256"
	"encoding/binary"
	"sort"
)

// Pick returns the `n` member peer IDs with the highest rendezvous scores
// for `key`. If len(candidates) ≤ n, returns candidates as-is. The result
// is sorted descending by score (so [0] is the most preferred holder).
func Pick(candidates []string, key string, n int) []string {
	if n <= 0 {
		return nil
	}
	if len(candidates) <= n {
		out := make([]string, len(candidates))
		copy(out, candidates)
		// Sort by score so callers get a stable, well-defined order even
		// when the group is small.
		sortByScore(out, key)
		return out
	}
	scored := make([]string, len(candidates))
	copy(scored, candidates)
	sortByScore(scored, key)
	return scored[:n]
}

// IndexOf returns the position a peer would occupy in Pick's result, or
// -1 if the peer isn't a candidate. Useful for migration: when a holder
// goes offline, the spare with the next-highest score takes its slot.
func IndexOf(candidates []string, key, peer string) int {
	scored := make([]string, len(candidates))
	copy(scored, candidates)
	sortByScore(scored, key)
	for i, p := range scored {
		if p == peer {
			return i
		}
	}
	return -1
}

func sortByScore(peers []string, key string) {
	sort.Slice(peers, func(i, j int) bool {
		return weight(peers[i], key) > weight(peers[j], key)
	})
}

func weight(peer, key string) uint64 {
	h := sha256.New()
	h.Write([]byte(peer))
	h.Write([]byte{0x00}) // separator so peer="ab" key="c" != peer="a" key="bc"
	h.Write([]byte(key))
	sum := h.Sum(nil)
	return binary.BigEndian.Uint64(sum[:8])
}
