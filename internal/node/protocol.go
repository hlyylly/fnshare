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

	"github.com/fxamacker/cbor/v2"
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

	// If we're not admin, we can't sign admission ourselves — but the
	// invite the joiner presented IS admin-signed. Cache the invite as
	// proof-of-admission so the eventual member-sync to admin (and
	// other peers) carries verifiable evidence that this peer was
	// authorized to join.
	var inviteRaw []byte
	if !g.IsAdminNode {
		inviteRaw, _ = cbor.Marshal(req.Invite)
	}

	m := &group.Member{
		PeerID:          remotePeer.String(),
		Nickname:        req.Nickname,
		NodePub:         req.NodePub,
		EncPub:          req.EncPub,
		ContributedB:    req.Contributed,
		JoinedAt:        now,
		AdmittedBySig:   resp.AdmitSig,
		AdmissionInvite: inviteRaw,
		LastAddrs:       multiaddrsToStrings(s.Conn().RemoteMultiaddr().String()),
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

// MembersRequest asks "give me the membership table of group <gid>". The
// reply lets the caller verify each member's admit_sig against the group
// admin's pubkey before merging — so a malicious peer can't fake members.
type MembersRequest struct {
	GroupID string `json:"group_id"`
}

type MembersResponse struct {
	GroupID string         `json:"group_id"`
	Members []group.Member `json:"members"`
	Error   string         `json:"error,omitempty"`
}

// handleMembers serves the membership table for a given group, IF this
// node is part of that group. Used by SyncGroupMembers to propagate joins
// that didn't go through the admin (the new-member event then "gossips"
// out via this protocol).
func (n *Node) handleMembers(s network.Stream) {
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(joinTimeout))

	var req MembersRequest
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		_ = json.NewEncoder(s).Encode(MembersResponse{Error: "decode: " + err.Error()})
		return
	}
	if req.GroupID == "" {
		_ = json.NewEncoder(s).Encode(MembersResponse{Error: "group_id required"})
		return
	}
	if _, err := group.LoadByID(n.Store, req.GroupID); err != nil {
		_ = json.NewEncoder(s).Encode(MembersResponse{Error: "we are not in group " + req.GroupID[:12]})
		return
	}
	members, err := group.ListMembers(n.Store, req.GroupID)
	if err != nil {
		_ = json.NewEncoder(s).Encode(MembersResponse{Error: err.Error()})
		return
	}
	out := make([]group.Member, 0, len(members))
	for _, m := range members {
		out = append(out, *m)
	}
	_ = json.NewEncoder(s).Encode(MembersResponse{GroupID: req.GroupID, Members: out})
}

// FetchMembersFrom asks `target` for the current membership table of
// `groupID`. Returns the slice for the caller to verify + merge.
func (n *Node) FetchMembersFrom(ctx context.Context, target peer.ID, groupID string) ([]group.Member, error) {
	ctx, cancel := context.WithTimeout(ctx, joinTimeout)
	defer cancel()

	s, err := n.Host.NewStream(ctx, target, ProtoMembers)
	if err != nil {
		return nil, fmt.Errorf("open members stream: %w", err)
	}
	defer s.Close()

	if err := json.NewEncoder(s).Encode(MembersRequest{GroupID: groupID}); err != nil {
		return nil, err
	}
	if err := s.CloseWrite(); err != nil {
		return nil, err
	}
	var resp MembersResponse
	if err := json.NewDecoder(s).Decode(&resp); err != nil {
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	return resp.Members, nil
}

// SyncGroupMembers asks every connected peer for each of our groups'
// member lists, verifies each member's admit signature against the group
// admin's pubkey, and merges new ones into our local table.
//
// This is what lets admin (alice) eventually learn about a member who
// joined via a non-admin (e.g. carol joining via bob when alice's
// IPv6/IPv4 was unreachable from carol's network).
//
// Verification: an admin admit signature is over (groupID|peerID|joinedAt).
// Forging requires the admin's private key, so accepting any sig that
// verifies against AdminPub is safe — a malicious gossiper can't smuggle
// in fake members.
func (n *Node) SyncGroupMembers(ctx context.Context) {
	groups, err := group.ListGroups(n.Store)
	if err != nil {
		return
	}
	connected := n.Host.Network().Peers()
	if len(connected) == 0 {
		return
	}
	self := n.Host.ID().String()
	for _, g := range groups {
		// Build a quick set of peer IDs we already know in this group so
		// we don't bother re-verifying / re-writing every tick.
		known := map[string]bool{}
		if existing, err := group.ListMembers(n.Store, g.ID); err == nil {
			for _, m := range existing {
				known[m.PeerID] = true
			}
		}

		added := 0
		for _, pid := range connected {
			members, err := n.FetchMembersFrom(ctx, pid, g.ID)
			if err != nil {
				continue
			}
			for _, m := range members {
				if known[m.PeerID] || m.PeerID == self {
					continue
				}
				// Verify proof-of-admission. Two acceptable proofs:
				//   (a) AdmittedBySig — admin's Ed25519 sig over (gid|peer|joined)
				//   (b) AdmissionInvite — admin-signed invite this peer used to join
				// Either ultimately chains back to admin's private key, so a
				// dishonest peer can't fake admission.
				if !g.VerifyMembership(&m) {
					n.Log.Warnw("member-sync: rejected member with no valid admission proof",
						"group", g.ID[:12], "peer", m.PeerID, "from", pid)
					continue
				}
				mm := m
				if err := group.PutMember(n.Store, g.ID, &mm); err != nil {
					n.Log.Warnw("member-sync: persist failed",
						"group", g.ID[:12], "peer", m.PeerID, "err", err)
					continue
				}
				known[m.PeerID] = true
				added++
				n.Log.Infow("member-sync: learned new member",
					"group", g.ID[:12], "nickname", m.Nickname, "peer", m.PeerID, "from", pid)
			}
		}
		if added > 0 {
			n.Log.Infow("member-sync round complete", "group", g.ID[:12], "added", added)
		}
	}
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
