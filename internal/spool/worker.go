package spool

import (
	"context"
	"time"

	"github.com/fnshare/fnshare/internal/ledger"

	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
)

// Sender is what the worker uses to actually deliver queued items. The
// node satisfies it (PutShardOnPeer + PutManifestRawOnPeer).
type Sender interface {
	PutShardOnPeer(ctx context.Context, p peer.ID, shardID string, data []byte) error
	PutManifestRawOnPeer(ctx context.Context, p peer.ID, fileID string, raw []byte) error
}

// Worker runs in the background, polling the spool on `interval` and
// flushing to peers the ledger has marked online.
type Worker struct {
	spool    *Spool
	sender   Sender
	ledger   *ledger.Ledger
	interval time.Duration
	log      *zap.SugaredLogger
}

func NewWorker(s *Spool, sender Sender, l *ledger.Ledger, interval time.Duration, log *zap.SugaredLogger) *Worker {
	return &Worker{spool: s, sender: sender, ledger: l, interval: interval, log: log}
}

// Run blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	// One immediate pass so anything spooled before daemon start drains.
	w.flushAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w.flushAll(ctx)
		}
	}
}

func (w *Worker) flushAll(ctx context.Context) {
	peers, err := w.spool.PendingPeers()
	if err != nil {
		w.log.Warnw("spool: list pending peers", "err", err)
		return
	}
	for _, p := range peers {
		if !w.ledger.IsOnline(p) {
			continue
		}
		w.flushPeer(ctx, p)
	}
}

func (w *Worker) flushPeer(ctx context.Context, peerID string) {
	pid, err := peer.Decode(peerID)
	if err != nil {
		w.log.Warnw("spool: bad peer id", "peer", peerID, "err", err)
		return
	}
	entries, err := w.spool.ListEntries(peerID)
	if err != nil {
		w.log.Warnw("spool: list entries", "peer", peerID, "err", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	// Send shards first (manifests reference shard ids), then manifests.
	// Within each kind, order is filesystem-determined.
	sent, failed := 0, 0
	for _, kind := range []Kind{KindShard, KindManifest} {
		for _, e := range entries {
			if e.Kind != kind {
				continue
			}
			data, err := w.spool.Read(e)
			if err != nil {
				w.log.Warnw("spool: read entry", "entry", e.String(), "err", err)
				failed++
				continue
			}

			ctx2, cancel := context.WithTimeout(ctx, 60*time.Second)
			switch e.Kind {
			case KindShard:
				err = w.sender.PutShardOnPeer(ctx2, pid, e.Key, data)
			case KindManifest:
				err = w.sender.PutManifestRawOnPeer(ctx2, pid, e.Key, data)
			}
			cancel()

			if err != nil {
				// Peer might have just gone offline again. Stop trying
				// this peer for now — next tick will retry.
				w.log.Debugw("spool flush failed", "entry", e.String(), "err", err)
				failed++
				return
			}
			if err := w.spool.Remove(e); err != nil {
				w.log.Warnw("spool: remove after send", "entry", e.String(), "err", err)
			}
			sent++
		}
	}
	if sent > 0 {
		w.log.Infow("spool flushed", "peer", peerID, "sent", sent, "failed", failed)
	}
}
