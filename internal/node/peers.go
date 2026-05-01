package node

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/fnshare/fnshare/internal/group"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multiaddr"
)

// ProtoPeers lets a node ask any group member "give me everyone's addresses".
// This is how nodes that joined via the same admin discover each other —
// without it, bob and carol (both onboarded via alice) have no way to dial
// each other when alice goes offline, and EC failure tolerance is wasted.
//
// Wire format: empty request, JSON response { "peers": { peerID: [maddr,…] } }.
const ProtoPeers protocol.ID = "/fnshare/peers/1.0.0"

type peersResponse struct {
	Peers map[string][]string `json:"peers"`
}

func (n *Node) registerPeersProto() {
	n.Host.SetStreamHandler(ProtoPeers, n.handlePeers)
}

func (n *Node) handlePeers(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(10 * time.Second))

	out := peersResponse{Peers: map[string][]string{}}

	// Walk every member across every group on this node and report the
	// addresses our peerstore knows about. The receiver may filter further.
	members, _ := group.AllMembersAcrossGroups(n.Store)
	for _, m := range members {
		pid, err := peer.Decode(m.PeerID)
		if err != nil {
			continue
		}
		addrs := n.Host.Peerstore().Addrs(pid)
		if len(addrs) == 0 {
			continue
		}
		strs := make([]string, 0, len(addrs))
		for _, a := range addrs {
			strs = append(strs, a.String())
		}
		out.Peers[m.PeerID] = strs
	}
	_ = json.NewEncoder(s).Encode(out)
}

// FetchPeersFrom asks `target` for its known peer addresses and adds them
// to our local peerstore. Returns the number of new addresses added.
func (n *Node) FetchPeersFrom(ctx context.Context, target peer.ID) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	s, err := n.Host.NewStream(ctx, target, ProtoPeers)
	if err != nil {
		return 0, fmt.Errorf("open peers stream: %w", err)
	}
	defer s.Close()

	if err := s.CloseWrite(); err != nil {
		return 0, err
	}
	var resp peersResponse
	if err := json.NewDecoder(s).Decode(&resp); err != nil {
		return 0, err
	}

	added := 0
	for pidStr, addrStrs := range resp.Peers {
		pid, err := peer.Decode(pidStr)
		if err != nil || pid == n.Host.ID() {
			continue
		}
		for _, a := range addrStrs {
			ma, err := multiaddr.NewMultiaddr(a)
			if err != nil {
				continue
			}
			n.Host.Peerstore().AddAddr(pid, ma, time.Hour)
			added++
		}
	}
	return added, nil
}
