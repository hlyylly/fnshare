// Package repair migrates a file's holdings from an offline peer to an
// online spare so EC redundancy doesn't permanently degrade.
//
// M7: with multi-stripe manifests, an offline peer holds shard-slot i for
// EVERY stripe of every file they're a holder of. Migration is therefore
// "all stripes' shard[i] from offline peer → spare", done by EC-
// reconstructing each stripe from the k surviving slots' shards.
//
// We work entirely in ciphertext: no file keys involved, so a repair
// goroutine has no extra crypto privilege beyond what holders already do.
package repair

import (
	"context"
	"fmt"
	"time"

	"github.com/fnshare/fnshare/internal/blockstore"
	"github.com/fnshare/fnshare/internal/ec"
	"github.com/fnshare/fnshare/internal/group"
	"github.com/fnshare/fnshare/internal/ledger"
	"github.com/fnshare/fnshare/internal/manifest"
	"github.com/fnshare/fnshare/internal/store"

	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
)

// Mover talks to the network. The node satisfies it.
type Mover interface {
	SelfID() peer.ID
	GetShardFromPeer(ctx context.Context, p peer.ID, shardID string) ([]byte, error)
	PutShardOnPeer(ctx context.Context, p peer.ID, shardID string, data []byte) error
	PutManifestOnPeer(ctx context.Context, p peer.ID, m *manifest.Manifest) error
}

type Service struct {
	mover      Mover
	store      *store.Store
	blockstore *blockstore.Store
	ledger     *ledger.Ledger
	log        *zap.SugaredLogger
}

func New(m Mover, s *store.Store, bs *blockstore.Store, l *ledger.Ledger, log *zap.SugaredLogger) *Service {
	return &Service{mover: m, store: s, blockstore: bs, ledger: l, log: log}
}

// ScanForOfflinePeer walks every manifest, locates files where the offline
// peer was a holder, and tries to migrate every stripe's shard[slot] to a
// spare member of the same group. Bounded total time.
func (s *Service) ScanForOfflinePeer(ctx context.Context, offlinePeer string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	manifests, err := manifest.List(s.store)
	if err != nil {
		s.log.Warnw("repair: list manifests", "err", err)
		return
	}
	for _, m := range manifests {
		slot := indexOf(m.Holders, offlinePeer)
		if slot < 0 {
			continue
		}
		if err := s.repairFile(ctx, m, slot); err != nil {
			s.log.Warnw("repair attempt", "file", m.FileID[:12], "err", err)
		}
	}
}

func (s *Service) repairFile(ctx context.Context, m *manifest.Manifest, slot int) error {
	// Are we even in the danger zone? We become at-risk when ≤ k of the
	// k+m holder slots are still online.
	live := 0
	self := s.mover.SelfID().String()
	for _, h := range m.Holders {
		if h == self || s.ledger.IsOnline(h) {
			live++
		}
	}
	if live > m.DataShards {
		s.log.Debugw("repair: file still has redundancy headroom",
			"file", m.FileID[:12], "live", live, "k", m.DataShards)
		return nil
	}

	// Find a spare: same-group member, not already a holder, online (or self).
	members, err := group.ListMembers(s.store, m.GroupID)
	if err != nil {
		return fmt.Errorf("list members: %w", err)
	}
	busy := map[string]bool{}
	for _, h := range m.Holders {
		busy[h] = true
	}
	spare := ""
	for _, mem := range members {
		if busy[mem.PeerID] {
			continue
		}
		if mem.PeerID != self && !s.ledger.IsOnline(mem.PeerID) {
			continue
		}
		spare = mem.PeerID
		break
	}
	if spare == "" {
		s.log.Infow("repair: no spare in group — file at risk",
			"file", m.FileID[:12], "group", m.GroupID[:12])
		return nil
	}

	// Migrate every stripe's shard at this slot.
	params := ec.Params{DataShards: m.DataShards, ParityShards: m.ParityShards}
	migrated := 0
	for _, stripe := range m.Stripes {
		if err := s.migrateStripeShard(ctx, m, &stripe, slot, spare, params); err != nil {
			s.log.Warnw("repair: stripe migration failed",
				"file", m.FileID[:12], "stripe", stripe.Index, "err", err)
			return err
		}
		migrated++
	}

	// Update manifest's holder slot, persist, propagate.
	m.Holders[slot] = spare
	if err := manifest.Put(s.store, m); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	for _, h := range m.Holders {
		if h == self {
			continue
		}
		pid, err := peer.Decode(h)
		if err != nil {
			continue
		}
		if err := s.mover.PutManifestOnPeer(ctx, pid, m); err != nil {
			s.log.Debugw("repair: propagate manifest", "to", h, "err", err)
		}
	}
	s.log.Infow("repair: migrated file",
		"file", m.FileID[:12], "stripes", migrated, "from", slotPeer(m, slot), "to", spare)
	return nil
}

// migrateStripeShard reconstructs stripe.ShardIDs[slot] via EC over k of
// the surviving slots' shards, then ships it to the spare.
func (s *Service) migrateStripeShard(ctx context.Context, m *manifest.Manifest,
	stripe *manifest.Stripe, slot int, spare string, params ec.Params) error {

	total := params.DataShards + params.ParityShards
	if len(stripe.ShardIDs) != total {
		return fmt.Errorf("stripe %d malformed", stripe.Index)
	}

	// Gather k shards from any slot EXCEPT the offline one.
	shardBytes := make([][]byte, total)
	gathered := 0
	self := s.mover.SelfID().String()
	for i := 0; i < total && gathered < params.DataShards; i++ {
		if i == slot {
			continue
		}
		shardID := stripe.ShardIDs[i]
		if data, err := s.blockstore.Get(shardID); err == nil {
			shardBytes[i] = data
			gathered++
			continue
		}
		holder := m.Holders[i]
		if holder == self || !s.ledger.IsOnline(holder) {
			continue
		}
		pid, err := peer.Decode(holder)
		if err != nil {
			continue
		}
		data, err := s.mover.GetShardFromPeer(ctx, pid, shardID)
		if err != nil {
			continue
		}
		shardBytes[i] = data
		gathered++
	}
	if gathered < params.DataShards {
		return fmt.Errorf("only %d of %d required shards available — cannot reconstruct",
			gathered, params.DataShards)
	}

	// Reconstruct slot's shard via EC. params.Decode rebuilds missing
	// shards in-place; we read out the slot we wanted.
	if _, err := params.Decode(shardBytes, int64(stripe.CiphertextBytes)); err != nil {
		return fmt.Errorf("ec reconstruct: %w", err)
	}
	if shardBytes[slot] == nil {
		return fmt.Errorf("ec reconstruct produced no shard at slot %d", slot)
	}
	// Verify the reconstructed shard's hash matches the manifest's
	// recorded id — paranoid, but cheap and surfaces RS bugs.
	if blockstore.HashOf(shardBytes[slot]) != stripe.ShardIDs[slot] {
		return fmt.Errorf("reconstructed shard hash mismatch at slot %d", slot)
	}

	// Place on spare.
	if spare == self {
		return s.blockstore.Put(stripe.ShardIDs[slot], shardBytes[slot])
	}
	pid, err := peer.Decode(spare)
	if err != nil {
		return err
	}
	return s.mover.PutShardOnPeer(ctx, pid, stripe.ShardIDs[slot], shardBytes[slot])
}

// ----- helpers -----

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func slotPeer(m *manifest.Manifest, slot int) string {
	if slot < 0 || slot >= len(m.Holders) {
		return "?"
	}
	return m.Holders[slot]
}
