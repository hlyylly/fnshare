package node

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/fnshare/fnshare/internal/config"
	"github.com/fnshare/fnshare/internal/group"
	"github.com/fnshare/fnshare/internal/keys"
	"github.com/fnshare/fnshare/internal/ledger"
	"github.com/fnshare/fnshare/internal/store"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
)

type Node struct {
	Cfg      config.Config
	Identity *keys.Identity
	Store    *store.Store
	Host     host.Host
	DHT      *dht.IpfsDHT
	Log      *zap.SugaredLogger

	mu     sync.RWMutex
	blocks blockstoreIface // nil until AttachBlockstore is called
	ledger *ledger.Ledger  // nil until AttachLedger is called
}

func (n *Node) AttachLedger(l *ledger.Ledger) {
	n.mu.Lock()
	n.ledger = l
	n.mu.Unlock()
}

func (n *Node) Ledger() *ledger.Ledger {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.ledger
}

// Groups returns every group this node is currently a member of.
func (n *Node) Groups() []*group.Group {
	gs, _ := group.ListGroups(n.Store)
	return gs
}

// SelfID is the node's libp2p peer id. Exposed so other packages can refer
// to "us" without importing libp2p directly.
func (n *Node) SelfID() peer.ID { return n.Host.ID() }

// blockstoreIface is the subset of *blockstore.Store that node uses. Defined
// inline to avoid an import cycle when blockstore eventually wants to log
// through the node logger.
type blockstoreIface interface {
	Put(shardID string, data []byte) error
	Get(shardID string) ([]byte, error)
	Has(shardID string) bool
	Delete(shardID string) error
}

// Blocks returns the attached blockstore, or nil.
func (n *Node) Blocks() blockstoreIface {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.blocks
}

type Options struct {
	Cfg      config.Config
	Identity *keys.Identity
	Store    *store.Store
	Log      *zap.SugaredLogger
}

func New(ctx context.Context, opts Options) (*Node, error) {
	listen := make([]multiaddr.Multiaddr, 0, len(opts.Cfg.ListenAddrs))
	for _, a := range opts.Cfg.ListenAddrs {
		ma, err := multiaddr.NewMultiaddr(a)
		if err != nil {
			return nil, fmt.Errorf("bad listen addr %q: %w", a, err)
		}
		listen = append(listen, ma)
	}

	libp2pOpts := []libp2p.Option{
		libp2p.Identity(opts.Identity.PrivKey),
		libp2p.ListenAddrs(listen...),
		libp2p.DefaultTransports,
		libp2p.DefaultMuxers,
		libp2p.DefaultSecurity,
		libp2p.NATPortMap(),
		libp2p.EnableNATService(),
		libp2p.EnableHolePunching(),
		libp2p.EnableRelay(),
	}

	if len(opts.Cfg.AnnounceAddrs) > 0 {
		announce := make([]multiaddr.Multiaddr, 0, len(opts.Cfg.AnnounceAddrs))
		for _, a := range opts.Cfg.AnnounceAddrs {
			ma, err := multiaddr.NewMultiaddr(a)
			if err != nil {
				return nil, fmt.Errorf("bad announce addr %q: %w", a, err)
			}
			announce = append(announce, ma)
		}
		libp2pOpts = append(libp2pOpts, libp2p.AddrsFactory(func(_ []multiaddr.Multiaddr) []multiaddr.Multiaddr {
			return announce
		}))
	}

	h, err := libp2p.New(libp2pOpts...)
	if err != nil {
		return nil, fmt.Errorf("libp2p host: %w", err)
	}

	kdht, err := dht.New(ctx, h, dht.Mode(dht.ModeAuto))
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("dht: %w", err)
	}
	if err := kdht.Bootstrap(ctx); err != nil {
		opts.Log.Warnw("dht bootstrap (non-fatal)", "err", err)
	}

	n := &Node{
		Cfg:      opts.Cfg,
		Identity: opts.Identity,
		Store:    opts.Store,
		Host:     h,
		DHT:      kdht,
		Log:      opts.Log,
	}

	n.registerProtocols()
	n.registerPeersProto()
	n.registerPingProto()
	return n, nil
}

// BootstrapAllGroups dials the bootstrap addresses of every group on this
// node, then asks every connected peer for its address book and merges the
// result. Safe to call from a daemon goroutine after startup.
func (n *Node) BootstrapAllGroups(ctx context.Context) {
	addrs, _ := group.AllBootstrapAddrs(n.Store)
	for _, a := range addrs {
		ma, err := multiaddr.NewMultiaddr(a)
		if err != nil {
			continue
		}
		ai, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			continue
		}
		if ai.ID == n.Host.ID() {
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = n.Host.Connect(dialCtx, *ai)
		cancel()
	}
	for _, p := range n.Host.Network().Peers() {
		added, err := n.FetchPeersFrom(ctx, p)
		if err != nil {
			n.Log.Debugw("fetch peers", "from", p, "err", err)
			continue
		}
		if added > 0 {
			n.Log.Infow("learned peer addresses", "from", p, "added", added)
		}
	}
}

func (n *Node) Close() error {
	if n.DHT != nil {
		_ = n.DHT.Close()
	}
	return n.Host.Close()
}

// SelfMultiaddrs returns the host's listen addrs with /p2p/<peerid> appended,
// suitable for embedding in invite links.
func (n *Node) SelfMultiaddrs() []string {
	pidPart, err := multiaddr.NewMultiaddr("/p2p/" + n.Host.ID().String())
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(n.Host.Addrs()))
	for _, a := range n.Host.Addrs() {
		out = append(out, a.Encapsulate(pidPart).String())
	}
	return out
}

// ConnectToPeers dials each multiaddr and adds it to the peerstore.
// Returns the first peer ID successfully connected, or an error if none worked.
func (n *Node) ConnectToPeers(ctx context.Context, addrs []string) (peer.ID, error) {
	var lastErr error
	for _, a := range addrs {
		ma, err := multiaddr.NewMultiaddr(a)
		if err != nil {
			lastErr = err
			continue
		}
		ai, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			lastErr = err
			continue
		}
		if err := n.Host.Connect(ctx, *ai); err != nil {
			n.Log.Warnw("connect failed", "peer", ai.ID, "err", err)
			lastErr = err
			continue
		}
		return ai.ID, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no bootstrap peers provided")
	}
	return "", lastErr
}
