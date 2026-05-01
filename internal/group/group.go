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
