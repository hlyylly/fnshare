package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/fnshare/fnshare/internal/group"
	"github.com/fnshare/fnshare/internal/invite"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

const (
	ProtoJoin    protocol.ID = "/fnshare/join/1.0.0"
	ProtoMembers protocol.ID = "/fnshare/members/1.0.0"

	joinTimeout = 30 * time.Second
)

// JoinRequest is sent by a prospective member to any current member.
// The current member verifies the embedded invite (signed by the group admin),
// records the new member, and replies with the group descriptor + member list.
type JoinRequest struct {
	Invite      *invite.Invite `json:"invite"`
	Nickname    string         `json:"nickname"`
	Contributed int64          `json:"contributed_bytes"`
	NodePub     []byte         `json:"node_pub"`
	EncPub      []byte         `json:"enc_pub,omitempty"` // joiner's X25519 pubkey
}

type JoinResponse struct {
	OK             bool           `json:"ok"`
	Error          string         `json:"error,omitempty"`
	GroupID        string         `json:"group_id,omitempty"`
	GroupName      string         `json:"group_name,omitempty"`
	GroupAdmin     []byte         `json:"group_admin,omitempty"`
	GroupSharedKey []byte         `json:"group_shared_key,omitempty"` // belt-and-suspenders: joiner already has it from the invite
	AdmissionAt    int64          `json:"admission_at,omitempty"`     // unix nano
	AdmitSig       []byte         `json:"admit_sig,omitempty"`        // signed by group admin
	Members        []group.Member `json:"members,omitempty"`
}

func (n *Node) registerProtocols() {
	n.Host.SetStreamHandler(ProtoJoin, n.handleJoin)
	n.Host.SetStreamHandler(ProtoMembers, n.handleMembers)
}

// handleJoin runs when a new node tries to join one of the groups we belong
// to. The invite carries the group ID; we look that group up in our store
// and reject if we're not part of it.
func (n *Node) handleJoin(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(joinTimeout))

	var req JoinRequest
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		writeJoinErr(s, fmt.Errorf("decode request: %w", err))
		return
	}
	if req.Invite == nil {
		writeJoinErr(s, errors.New("invite required"))
		return
	}
	if err := req.Invite.Verify(); err != nil {
		writeJoinErr(s, fmt.Errorf("invite invalid: %w", err))
		return
	}

	g, err := group.LoadByID(n.Store, req.Invite.GroupID)
	if err != nil {
		writeJoinErr(s, fmt.Errorf("we are not part of group %s", req.Invite.GroupID))
		return
	}

	remotePeer := s.Conn().RemotePeer()
	now := time.Now().UTC()

	resp := JoinResponse{
		OK:             true,
		GroupID:        g.ID,
		GroupName:      g.Name,
		GroupAdmin:     g.AdminPub,
		GroupSharedKey: g.SharedKey,
	}
	if g.IsAdminNode {
		sig, err := g.AdmitMember(remotePeer.String(), now)
		if err != nil {
			writeJoinErr(s, err)
			return
		}
		resp.AdmissionAt = now.UnixNano()
		resp.AdmitSig = sig
	}

	m := &group.Member{
		PeerID:        remotePeer.String(),
		Nickname:      req.Nickname,
		NodePub:       req.NodePub,
		EncPub:        req.EncPub,
		ContributedB:  req.Contributed,
		JoinedAt:      now,
		AdmittedBySig: resp.AdmitSig,
		LastAddrs:     multiaddrsToStrings(s.Conn().RemoteMultiaddr().String()),
	}
	if err := group.PutMember(n.Store, g.ID, m); err != nil {
		writeJoinErr(s, fmt.Errorf("persist member: %w", err))
		return
	}

	members, _ := group.ListMembers(n.Store, g.ID)
	for _, mm := range members {
		resp.Members = append(resp.Members, *mm)
	}

	if err := json.NewEncoder(s).Encode(resp); err != nil {
		n.Log.Warnw("write join response", "err", err)
		return
	}
	n.Log.Infow("admitted new member",
		"group", g.ID[:12], "peer", remotePeer, "nickname", req.Nickname)
}

// handleMembers serves the union of all group members on this node. M5
// will replace this with per-group sync.
func (n *Node) handleMembers(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(joinTimeout))

	members, err := group.AllMembersAcrossGroups(n.Store)
	if err != nil {
		_ = json.NewEncoder(s).Encode(map[string]string{"error": err.Error()})
		return
	}
	out := make([]group.Member, 0, len(members))
	for _, m := range members {
		out = append(out, *m)
	}
	_ = json.NewEncoder(s).Encode(map[string]any{"members": out})
}

// JoinViaPeer is the client side of /fnshare/join/1.0.0. The local node opens
// a stream to `target`, ships the JoinRequest, and applies the response by
// installing the group descriptor and the member list locally.
func (n *Node) JoinViaPeer(ctx context.Context, target peer.ID, inv *invite.Invite) (*JoinResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()

	s, err := n.Host.NewStream(ctx, target, ProtoJoin)
	if err != nil {
		return nil, fmt.Errorf("open join stream: %w", err)
	}
	defer s.Close()

	pubRaw, err := n.Identity.PubKey.Raw()
	if err != nil {
		return nil, err
	}

	req := JoinRequest{
		Invite:      inv,
		Nickname:    n.Cfg.Nickname,
		Contributed: n.Cfg.ContributedBytes,
		NodePub:     pubRaw,
		EncPub:      n.Identity.EncPub,
	}
	if err := json.NewEncoder(s).Encode(req); err != nil {
		return nil, fmt.Errorf("write join req: %w", err)
	}

	var resp JoinResponse
	if err := json.NewDecoder(s).Decode(&resp); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode join resp: %w", err)
	}
	if !resp.OK {
		return nil, fmt.Errorf("remote refused: %s", resp.Error)
	}

	// Install group locally as a non-admin replica. Prefer the shared key
	// from the invite (we already validated its signature); fall back to
	// the join response in case the invite predates the field.
	sharedKey := inv.GroupSharedKey
	if len(sharedKey) == 0 {
		sharedKey = resp.GroupSharedKey
	}
	g := &group.Group{
		ID:          resp.GroupID,
		Name:        resp.GroupName,
		AdminPub:    resp.GroupAdmin,
		SharedKey:   sharedKey,
		IsAdminNode: false,
	}
	if err := group.Save(n.Store, g); err != nil {
		return nil, fmt.Errorf("persist group: %w", err)
	}

	for _, m := range resp.Members {
		mm := m
		if err := group.PutMember(n.Store, g.ID, &mm); err != nil {
			n.Log.Warnw("persist member", "peer", m.PeerID, "err", err)
		}
	}
	return &resp, nil
}

func writeJoinErr(s network.Stream, err error) {
	_ = json.NewEncoder(s).Encode(JoinResponse{OK: false, Error: err.Error()})
}

func multiaddrsToStrings(in ...string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}
