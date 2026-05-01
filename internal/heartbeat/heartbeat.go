// Package heartbeat probes every group member periodically. Failures
// accumulate in the ledger; once a peer crosses the offline threshold the
// callback is invoked so the daemon can schedule a lazy-repair scan.
package heartbeat

import (
	"context"
	"sync"
	"time"

	"github.com/fnshare/fnshare/internal/group"
	"github.com/fnshare/fnshare/internal/ledger"
	"github.com/fnshare/fnshare/internal/store"

	"github.com/libp2p/go-libp2p/core/peer"
	"go.uber.org/zap"
)

// Pinger is whatever can probe a remote peer. The node satisfies it via
// (*node.Node).Ping — declared as an interface so this package doesn't
// have to import node and create a cycle.
type Pinger interface {
	Ping(ctx context.Context, target peer.ID) error
	SelfID() peer.ID
}

// OnTransition fires when a peer's liveness changed. wentOffline=true
// means now offline; otherwise now online.
type OnTransition func(peerID string, wentOffline bool)

type Service struct {
	pinger   Pinger
	store    *store.Store
	ledger   *ledger.Ledger
	log      *zap.SugaredLogger
	interval time.Duration
	on       OnTransition

	mu       sync.Mutex
	stopOnce sync.Once
}

func New(p Pinger, s *store.Store, l *ledger.Ledger, interval time.Duration,
	on OnTransition, log *zap.SugaredLogger) *Service {
	return &Service{
		pinger: p, store: s, ledger: l, log: log,
		interval: interval, on: on,
	}
}

// Run blocks until ctx is cancelled. Pings every member of every group
// the local node belongs to, on `interval`. Failures accumulate in the
// ledger; transitions trigger the OnTransition callback.
func (s *Service) Run(ctx context.Context) {
	tick := time.NewTicker(s.interval)
	defer tick.Stop()

	// Run once immediately so the first round happens without waiting a
	// full interval after startup.
	s.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.scan(ctx)
		}
	}
}

func (s *Service) scan(ctx context.Context) {
	members, err := group.AllMembersAcrossGroups(s.store)
	if err != nil {
		s.log.Warnw("heartbeat: list members", "err", err)
		return
	}
	self := s.pinger.SelfID().String()

	// Probe in parallel — all RPCs short-timeout; bounded by concurrency
	// (how many at-a-time) so a 100-peer group doesn't fork 100 streams.
	const maxParallel = 8
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for _, m := range members {
		if m.PeerID == self {
			continue
		}
		pid, err := peer.Decode(m.PeerID)
		if err != nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(pid peer.ID, peerStr string) {
			defer wg.Done()
			defer func() { <-sem }()

			err := s.pinger.Ping(ctx, pid)
			result := s.ledger.RecordPing(peerStr, err == nil)
			if result.WentOffline {
				s.log.Warnw("peer went offline", "peer", peerStr, "rep", result.Reputation)
				if s.on != nil {
					s.on(peerStr, true)
				}
			} else if result.WentOnline {
				s.log.Infow("peer back online", "peer", peerStr, "rep", result.Reputation)
				if s.on != nil {
					s.on(peerStr, false)
				}
			}
		}(pid, m.PeerID)
	}
	wg.Wait()
}
