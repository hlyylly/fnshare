package node

import (
	"context"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// ProtoPing is a minimal liveness probe. The client opens a stream and
// closes it; the server accepts and closes. Anything succeeding here means
// the peer is responsive (libp2p handshake passed and a fnshare daemon is
// answering on this protocol).
//
// Not to be confused with libp2p's built-in /ipfs/ping protocol, which we
// could in theory reuse — but going through our own handler means a remote
// libp2p node WITHOUT fnshare doesn't accidentally count as "online" for
// our purposes.
const ProtoPing protocol.ID = "/fnshare/ping/1.0.0"

const pingTimeout = 5 * time.Second

func (n *Node) registerPingProto() {
	n.Host.SetStreamHandler(ProtoPing, func(s network.Stream) {
		// Read & discard one byte (or EOF), then close. The stream itself
		// being established is the only signal we need.
		_ = s.SetDeadline(time.Now().Add(pingTimeout))
		buf := make([]byte, 1)
		_, _ = s.Read(buf)
		_ = s.Close()
	})
}

// Ping returns nil if `target` is reachable on the ping protocol.
func (n *Node) Ping(ctx context.Context, target peer.ID) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	s, err := n.Host.NewStream(ctx, target, ProtoPing)
	if err != nil {
		return fmt.Errorf("open ping stream: %w", err)
	}
	defer s.Close()
	// Write a single byte to confirm the stream is bidirectionally usable
	// (catches half-broken connections that NewStream alone wouldn't).
	if _, err := s.Write([]byte{0x01}); err != nil {
		return fmt.Errorf("write ping: %w", err)
	}
	return nil
}
