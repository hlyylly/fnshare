package invite

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/fnshare/fnshare/internal/group"
	"github.com/fxamacker/cbor/v2"
)

// Scheme is the URL scheme used for invite links.
const Scheme = "fnshare"

// Invite is a self-contained, signed credential a prospective member can
// present to any current group node to be admitted.
//
// Wire format:
//   fnshare://join#<base64url(cbor(payload))>
type Invite struct {
	GroupID        string   `cbor:"gid"`            // hex of group pubkey
	GroupName      string   `cbor:"name"`
	GroupAdminPub  []byte   `cbor:"apub"`           // group pubkey, embedded for offline verification
	GroupSharedKey []byte   `cbor:"sk"`             // 32B AES-256 shared key (group secret)
	BootstrapPeers []string `cbor:"boot"`           // multiaddrs incl. /p2p/<peerid>
	Nonce          []byte   `cbor:"nonce"`          // 16 random bytes, joining node echoes back
	IssuedAt       int64    `cbor:"iat"`            // unix seconds
	ExpiresAt      int64    `cbor:"exp"`            // unix seconds
	QuotaCap       int64    `cbor:"quota,omitempty"`
	Signature      []byte   `cbor:"sig"`            // group-key sig over signedBytes()
}

// payload is the same as Invite minus the Signature, used as the message body.
type payload struct {
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

func (i *Invite) signedBytes() ([]byte, error) {
	return cbor.Marshal(payload{
		GroupID:        i.GroupID,
		GroupName:      i.GroupName,
		GroupAdminPub:  i.GroupAdminPub,
		GroupSharedKey: i.GroupSharedKey,
		BootstrapPeers: i.BootstrapPeers,
		Nonce:          i.Nonce,
		IssuedAt:       i.IssuedAt,
		ExpiresAt:      i.ExpiresAt,
		QuotaCap:       i.QuotaCap,
	})
}

// Create issues a fresh invite signed by the group admin key.
func Create(g *group.Group, bootstrap []string, ttl time.Duration, quotaCap int64) (*Invite, error) {
	if len(g.AdminPriv) != ed25519.PrivateKeySize {
		return nil, errors.New("only the admin node can create invites")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inv := &Invite{
		GroupID:        g.ID,
		GroupName:      g.Name,
		GroupAdminPub:  g.AdminPub,
		GroupSharedKey: g.SharedKey,
		BootstrapPeers: bootstrap,
		Nonce:          nonce,
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(ttl).Unix(),
		QuotaCap:       quotaCap,
	}
	body, err := inv.signedBytes()
	if err != nil {
		return nil, err
	}
	inv.Signature = ed25519.Sign(ed25519.PrivateKey(g.AdminPriv), body)
	return inv, nil
}

// Encode serializes an invite into the canonical URL form.
func (i *Invite) Encode() (string, error) {
	raw, err := cbor.Marshal(i)
	if err != nil {
		return "", err
	}
	return Scheme + "://join#" + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Decode parses an invite URL and verifies its signature.
func Decode(s string) (*Invite, error) {
	s = strings.TrimSpace(s)
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("parse invite url: %w", err)
	}
	if u.Scheme != Scheme {
		return nil, fmt.Errorf("expected scheme %q, got %q", Scheme, u.Scheme)
	}
	if u.Fragment == "" {
		return nil, errors.New("invite missing fragment payload")
	}
	raw, err := base64.RawURLEncoding.DecodeString(u.Fragment)
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	var inv Invite
	if err := cbor.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("cbor decode: %w", err)
	}
	if err := inv.Verify(); err != nil {
		return nil, err
	}
	return &inv, nil
}

// Verify checks signature validity and expiry. Caller is still responsible
// for replay-protection (tracking consumed nonces server-side).
func (i *Invite) Verify() error {
	if len(i.GroupAdminPub) != ed25519.PublicKeySize {
		return errors.New("invite: admin pubkey wrong size")
	}
	if time.Now().Unix() > i.ExpiresAt {
		return errors.New("invite: expired")
	}
	body, err := i.signedBytes()
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(i.GroupAdminPub), body, i.Signature) {
		return errors.New("invite: bad signature")
	}
	return nil
}
