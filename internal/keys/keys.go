package keys

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fnshare/fnshare/internal/crypto"

	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

type Identity struct {
	// Wire identity (libp2p stream auth + signatures): Ed25519.
	PrivKey libp2pcrypto.PrivKey
	PubKey  libp2pcrypto.PubKey
	PeerID  peer.ID

	// Encryption identity (file-key wrapping for "private" files): X25519.
	// Generated independently from the wire key; persisted alongside it.
	EncPub  []byte
	EncPriv []byte
}

func LoadOrCreate(path string) (*Identity, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		priv, _, err := libp2pcrypto.GenerateEd25519Key(nil)
		if err != nil {
			return nil, err
		}
		raw, err := libp2pcrypto.MarshalPrivateKey(priv)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return nil, err
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	priv, err := libp2pcrypto.UnmarshalPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("unmarshal identity key: %w", err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return nil, err
	}

	// Sibling X25519 keypair lives next to the wire key. Lazily created on
	// first run so old M3 nodes auto-upgrade.
	encPub, encPriv, err := loadOrCreateEncKeys(path + ".enc")
	if err != nil {
		return nil, err
	}
	return &Identity{
		PrivKey: priv, PubKey: priv.GetPublic(), PeerID: pid,
		EncPub: encPub, EncPriv: encPriv,
	}, nil
}

func loadOrCreateEncKeys(path string) (pub, priv []byte, err error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		pub, priv, err = crypto.NewEncKeypair()
		if err != nil {
			return nil, nil, err
		}
		// Layout: 32B pub || 32B priv.
		if err := os.WriteFile(path, append(append([]byte{}, pub...), priv...), 0o600); err != nil {
			return nil, nil, err
		}
		return pub, priv, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) != crypto.EncPubSize+crypto.EncPrivSize {
		return nil, nil, fmt.Errorf("enc key file %s has wrong length %d", path, len(raw))
	}
	return raw[:crypto.EncPubSize], raw[crypto.EncPubSize:], nil
}

// RawEd25519 returns the underlying 32-byte Ed25519 private key seed for
// signing operations outside the libp2p crypto interface (e.g. invite links).
func (id *Identity) RawEd25519() (ed25519.PrivateKey, ed25519.PublicKey, error) {
	raw, err := id.PrivKey.Raw()
	if err != nil {
		return nil, nil, err
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, nil, fmt.Errorf("expected ed25519 priv key (%d bytes), got %d", ed25519.PrivateKeySize, len(raw))
	}
	priv := ed25519.PrivateKey(raw)
	return priv, priv.Public().(ed25519.PublicKey), nil
}

func (id *Identity) Fingerprint() string {
	pubRaw, _ := id.PubKey.Raw()
	return hex.EncodeToString(pubRaw[:8])
}
