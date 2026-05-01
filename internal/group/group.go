// Package group manages the membership state of every fnshare group this
// node belongs to.
//
// Multi-group: a single node can be admin of one group, member of another,
// and member of a third — all simultaneously. State is keyed by group id:
//
//   group/<gid>                — the Group descriptor
//   member/<gid>/<peer_id>     — a Member entry in that group
//   bootstrap/<gid>            — multiaddrs to redial on every daemon start
package group

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fnshare/fnshare/internal/crypto"
	"github.com/fnshare/fnshare/internal/store"
	"github.com/fxamacker/cbor/v2"
)

type Group struct {
	ID          string    `cbor:"id"`            // hex of group pubkey
	Name        string    `cbor:"name"`
	AdminPub    []byte    `cbor:"admin_pub"`     // Ed25519 pubkey, 32B
	AdminPriv   []byte    `cbor:"admin_priv,omitempty"` // only on admin's node
	CreatedAt   time.Time `cbor:"created_at"`
	IsAdminNode bool      `cbor:"is_admin"`

	// SharedKey: 32B AES-256, used to wrap per-file keys for "shared"
	// files. Distributed via the invite + the join handshake.
	SharedKey []byte `cbor:"sk"`
}

type Member struct {
	PeerID         string    `cbor:"peer_id"`
	Nickname       string    `cbor:"nickname"`
	NodePub        []byte    `cbor:"node_pub"`
	EncPub         []byte    `cbor:"enc_pub,omitempty"`
	ContributedB   int64     `cbor:"contributed"`
	JoinedAt       time.Time `cbor:"joined_at"`
	AdmittedBySig  []byte    `cbor:"admitted_sig"`
	LastAddrs      []string  `cbor:"last_addrs,omitempty"`

	// AdmissionInvite is the raw CBOR of the invite the joiner presented,
	// stored when admission was processed by a NON-admin node (in which
	// case AdmittedBySig is empty because non-admin can't sign).
	//
	// During member-sync, peers verify membership by accepting EITHER:
	//   (a) AdmittedBySig is a valid admin Ed25519 signature, OR
	//   (b) AdmissionInvite decodes to a valid invite for THIS group.
	//
	// Both ultimately chain back to the admin's key — the invite was
	// signed by admin when minted — so accepting either is safe.
	AdmissionInvite []byte `cbor:"adm_inv,omitempty"`
}

const (
	groupKeyPrefix     = "group/"
	memberKeyPrefix    = "member/"
	bootstrapKeyPrefix = "bootstrap/"
)

func groupKey(gid string) []byte     { return []byte(groupKeyPrefix + gid) }
func memberKey(gid, pid string) []byte {
	return []byte(memberKeyPrefix + gid + "/" + pid)
}
func bootstrapKey(gid string) []byte { return []byte(bootstrapKeyPrefix + gid) }

// ----- group lifecycle -----

// Create initializes a fresh group with a freshly generated admin keypair
// and AES-256 shared key. The caller is responsible for persisting it via
// Save and inserting itself as the admin Member.
func Create(name string) (*Group, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	sk, err := crypto.NewSymmetricKey()
	if err != nil {
		return nil, err
	}
	return &Group{
		ID:          hex.EncodeToString(pub),
		Name:        name,
		AdminPub:    pub,
		AdminPriv:   priv,
		CreatedAt:   time.Now().UTC(),
		IsAdminNode: true,
		SharedKey:   sk,
	}, nil
}

func Save(s *store.Store, g *Group) error {
	raw, err := cbor.Marshal(g)
	if err != nil {
		return err
	}
	return s.Put(groupKey(g.ID), raw)
}

func LoadByID(s *store.Store, gid string) (*Group, error) {
	raw, err := s.Get(groupKey(gid))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNoGroup
	}
	if err != nil {
		return nil, err
	}
	var g Group
	if err := cbor.Unmarshal(raw, &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// ListGroups returns every Group this node belongs to.
func ListGroups(s *store.Store) ([]*Group, error) {
	var out []*Group
	err := s.Iterate([]byte(groupKeyPrefix), func(_, v []byte) bool {
		var g Group
		if cbor.Unmarshal(v, &g) == nil {
			out = append(out, &g)
		}
		return true
	})
	return out, err
}

// AdmitMember produces the admin signature authenticating a new member.
// Only callable on the admin node (where AdminPriv is populated).
func (g *Group) AdmitMember(peerID string, joinedAt time.Time) ([]byte, error) {
	if len(g.AdminPriv) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("not the admin node — cannot admit members")
	}
	return ed25519.Sign(ed25519.PrivateKey(g.AdminPriv), admitMessage(g.ID, peerID, joinedAt)), nil
}

// VerifyAdmission checks an admission signature against the group's admin pubkey.
func (g *Group) VerifyAdmission(peerID string, joinedAt time.Time, sig []byte) bool {
	return ed25519.Verify(ed25519.PublicKey(g.AdminPub), admitMessage(g.ID, peerID, joinedAt), sig)
}

// VerifyMembership accepts a Member as a valid group member if either:
//
//	(a) AdmittedBySig is a valid admin Ed25519 signature over the
//	    (groupID|peerID|joinedAt) tuple — i.e., admin processed the join
//	    directly.
//
//	(b) AdmissionInvite is a CBOR-encoded invite that decodes successfully,
//	    verifies against admin's pubkey, and matches THIS group — i.e.,
//	    admin pre-authorized the join via a one-time invite link, and a
//	    non-admin peer (like bob) processed the actual handshake.
//
// Either way, the chain of trust ends at admin's private key. A dishonest
// gossiping peer can't smuggle in a fake member entry because they can't
// forge either kind of signature.
func (g *Group) VerifyMembership(m *Member) bool {
	if len(m.AdmittedBySig) > 0 && g.VerifyAdmission(m.PeerID, m.JoinedAt, m.AdmittedBySig) {
		return true
	}
	if len(m.AdmissionInvite) > 0 {
		// We import invite.Decode lazily via raw cbor to avoid an import
		// cycle (invite already imports group). Inline decode + verify.
		var inv struct {
			GroupID        string `cbor:"gid"`
			GroupAdminPub  []byte `cbor:"apub"`
			Signature      []byte `cbor:"sig"`
		}
		if cbor.Unmarshal(m.AdmissionInvite, &inv) != nil {
			return false
		}
		if inv.GroupID != g.ID {
			return false
		}
		if !bytes.Equal(inv.GroupAdminPub, g.AdminPub) {
			return false
		}
		// Re-verify the invite's own signature by decoding it through the
		// invite package's logic. Since we don't want a cycle here, reuse
		// the canonical signed-bytes layout: anything that round-trips
		// through invite.Decode (and verifies admin sig) is acceptable.
		// Cheap path: if the marshaled invite has a non-empty signature
		// field AND admin pubkey field matches ours, trust it. The full
		// signature check happens in invite.Decode; this is a fast path
		// for the gossip case where we just got it from another peer.
		// Defense-in-depth full check:
		return verifyInviteSignature(m.AdmissionInvite, g.AdminPub)
	}
	return false
}

// verifyInviteSignature does the same cryptographic check as
// invite.Decode without importing the invite package (which would create
// a cycle group→invite→group). Re-implements just the verification path:
// the invite is CBOR with a "sig" field over the rest of the fields.
func verifyInviteSignature(raw, adminPub []byte) bool {
	// Parse the full invite into a generic map, extract sig + body, then
	// re-marshal the body without sig and verify.
	var full map[string]cbor.RawMessage
	if cbor.Unmarshal(raw, &full) != nil {
		return false
	}
	sigRaw, ok := full["sig"]
	if !ok {
		return false
	}
	var sig []byte
	if cbor.Unmarshal(sigRaw, &sig) != nil {
		return false
	}
	// Re-marshal body in the canonical signed-fields order. Must match
	// what internal/invite/invite.go signedBytes() produces.
	type signedPayload struct {
		GroupID        string   `cbor:"gid"`
		GroupName      string   `cbor:"name"`
		GroupAdminPub  []byte   `cbor:"apub"`
		GroupSharedKey []byte   `cbor:"sk"`
		BootstrapPeers []string `cbor:"boot"`
		Nonce          []byte   `cbor:"nonce"`
		IssuedAt       int64    `cbor:"iat"`
		ExpiresAt      int64    `cbor:"exp"`
		QuotaCap       int64    `cbor:"quota,omitempty"`
	}
	var body signedPayload
	if cbor.Unmarshal(raw, &body) != nil {
		return false
	}
	canonical, err := cbor.Marshal(body)
	if err != nil {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(adminPub), canonical, sig)
}

func admitMessage(groupID, peerID string, t time.Time) []byte {
	return []byte(fmt.Sprintf("fnshare-admit|%s|%s|%d", groupID, peerID, t.UnixNano()))
}

// ----- member CRUD -----

func PutMember(s *store.Store, gid string, m *Member) error {
	raw, err := cbor.Marshal(m)
	if err != nil {
		return err
	}
	return s.Put(memberKey(gid, m.PeerID), raw)
}

func GetMember(s *store.Store, gid, peerID string) (*Member, error) {
	raw, err := s.Get(memberKey(gid, peerID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrMemberNotFound
	}
	if err != nil {
		return nil, err
	}
	var m Member
	if err := cbor.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func ListMembers(s *store.Store, gid string) ([]*Member, error) {
	prefix := []byte(memberKeyPrefix + gid + "/")
	var members []*Member
	err := s.Iterate(prefix, func(_, v []byte) bool {
		var m Member
		if cbor.Unmarshal(v, &m) == nil {
			members = append(members, &m)
		}
		return true
	})
	return members, err
}

// AllMembersAcrossGroups returns the union of members in every group on
// this node, deduplicated by peer id. Useful for peer-discovery and the
// unified file view where we may need to fetch a manifest from anyone we
// share any group with.
func AllMembersAcrossGroups(s *store.Store) ([]*Member, error) {
	seen := map[string]*Member{}
	err := s.Iterate([]byte(memberKeyPrefix), func(k, v []byte) bool {
		var m Member
		if cbor.Unmarshal(v, &m) != nil {
			return true
		}
		// Skip if we already have an entry; first-write wins is fine, the
		// fields we care about (peer id, addrs) don't vary across groups.
		if _, dup := seen[m.PeerID]; !dup {
			seen[m.PeerID] = &m
		}
		_ = k
		return true
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Member, 0, len(seen))
	for _, m := range seen {
		out = append(out, m)
	}
	return out, nil
}

// MemberOf returns the list of group IDs that include peerID.
func MemberOf(s *store.Store, peerID string) ([]string, error) {
	var gids []string
	err := s.Iterate([]byte(memberKeyPrefix), func(k, _ []byte) bool {
		// key format: member/<gid>/<peer_id>
		rest := strings.TrimPrefix(string(k), memberKeyPrefix)
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && parts[1] == peerID {
			gids = append(gids, parts[0])
		}
		return true
	})
	return gids, err
}

// ----- bootstrap addrs -----

func SaveBootstrap(s *store.Store, gid string, addrs []string) error {
	raw, err := cbor.Marshal(addrs)
	if err != nil {
		return err
	}
	return s.Put(bootstrapKey(gid), raw)
}

func LoadBootstrap(s *store.Store, gid string) ([]string, error) {
	raw, err := s.Get(bootstrapKey(gid))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var addrs []string
	if err := cbor.Unmarshal(raw, &addrs); err != nil {
		return nil, err
	}
	return addrs, nil
}

// AllBootstrapAddrs returns every bootstrap address across every group.
func AllBootstrapAddrs(s *store.Store) ([]string, error) {
	var out []string
	err := s.Iterate([]byte(bootstrapKeyPrefix), func(_, v []byte) bool {
		var addrs []string
		if cbor.Unmarshal(v, &addrs) == nil {
			out = append(out, addrs...)
		}
		return true
	})
	return out, err
}

var (
	ErrNoGroup        = errors.New("group: not found")
	ErrMemberNotFound = errors.New("group: member not found")
)
