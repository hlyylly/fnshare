// Package crypto centralizes the encryption primitives fnshare uses.
//
// Two layers:
//
//  1. File content & filename: AES-256-GCM with a freshly generated 32-byte
//     "file key". The file key is the secret you need to read the file.
//
//  2. File key wrapping: a tiny envelope that hides the file key from
//     storage holders. Two modes:
//        - shared:  AES-256-GCM with the group's shared symmetric key.
//                   Anyone in the group can unwrap.
//        - private: NaCl anonymous box (X25519 + XSalsa20+Poly1305) targeting
//                   the owner's encryption pubkey. Only the owner — who
//                   alone holds the X25519 secret — can unwrap.
//
// The wire-level keys (libp2p host identity = Ed25519) are unrelated to
// these enc keys. Each node generates a separate X25519 keypair on init
// for the encryption layer.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/nacl/box"
)

const (
	// FileKeySize is the size of the per-file AES-256 key.
	FileKeySize = 32
	// EncPubSize / EncPrivSize are X25519 sizes.
	EncPubSize  = 32
	EncPrivSize = 32

	gcmNonceSize = 12
)

// NewFileKey returns a fresh random AES-256 file key.
func NewFileKey() ([]byte, error) {
	k := make([]byte, FileKeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// NewSymmetricKey returns 32 random bytes — used for the group shared key.
func NewSymmetricKey() ([]byte, error) {
	return NewFileKey()
}

// NewEncKeypair generates an X25519 keypair for the file-key wrapping layer.
func NewEncKeypair() (pub, priv []byte, err error) {
	pubArr, privArr, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	return pubArr[:], privArr[:], nil
}

// SealAES encrypts plaintext under a 32-byte AES-256 key. The 12-byte
// random nonce is prepended to the ciphertext so the caller doesn't need
// to track it separately.
func SealAES(key, plaintext []byte) ([]byte, error) {
	if len(key) != FileKeySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", FileKeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	out := make([]byte, 0, len(nonce)+len(ct))
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// OpenAES is the inverse of SealAES.
func OpenAES(key, ciphertext []byte) ([]byte, error) {
	if len(key) != FileKeySize {
		return nil, fmt.Errorf("crypto: key must be %d bytes, got %d", FileKeySize, len(key))
	}
	if len(ciphertext) < gcmNonceSize+1 {
		return nil, errors.New("crypto: ciphertext too short")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, ct := ciphertext[:gcmNonceSize], ciphertext[gcmNonceSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

// SealAnonymous wraps `plaintext` so that only the holder of the X25519
// private key matching `recipientPub` can read it. No sender key needed.
func SealAnonymous(plaintext, recipientPub []byte) ([]byte, error) {
	if len(recipientPub) != EncPubSize {
		return nil, fmt.Errorf("crypto: recipient pub must be %d bytes", EncPubSize)
	}
	var pubArr [32]byte
	copy(pubArr[:], recipientPub)
	return box.SealAnonymous(nil, plaintext, &pubArr, rand.Reader)
}

// OpenAnonymous is the inverse of SealAnonymous.
func OpenAnonymous(ciphertext, recipientPub, recipientPriv []byte) ([]byte, error) {
	if len(recipientPub) != EncPubSize || len(recipientPriv) != EncPrivSize {
		return nil, errors.New("crypto: bad recipient key sizes")
	}
	var pubArr, privArr [32]byte
	copy(pubArr[:], recipientPub)
	copy(privArr[:], recipientPriv)
	out, ok := box.OpenAnonymous(nil, ciphertext, &pubArr, &privArr)
	if !ok {
		return nil, errors.New("crypto: anonymous box decrypt failed")
	}
	return out, nil
}
