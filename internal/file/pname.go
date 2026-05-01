package file

import (
	"errors"

	"github.com/fnshare/fnshare/internal/store"
	"github.com/fxamacker/cbor/v2"
)

// pname stores plaintext filenames for the OWNER's private files. The
// manifest's Filename field holds an AES-GCM ciphertext (so holders can't
// see what the file is called); but on the owner's own node we want the
// real name in our FUSE mount and `fnshare ls` output without paying
// the cost of decrypting the file body just to read the filename.
//
// Stored only on the owner's node, never replicated.
//
// Key: "pname/<file_id>" → plaintext filename string (CBOR).

const pnameKeyPrefix = "pname/"

func pnameKey(fileID string) []byte { return []byte(pnameKeyPrefix + fileID) }

func savePrivateName(s *store.Store, fileID, name string) error {
	if name == "" {
		return nil
	}
	raw, err := cbor.Marshal(name)
	if err != nil {
		return err
	}
	return s.Put(pnameKey(fileID), raw)
}

func loadPrivateName(s *store.Store, fileID string) (string, bool) {
	raw, err := s.Get(pnameKey(fileID))
	if errors.Is(err, store.ErrNotFound) || err != nil {
		return "", false
	}
	var name string
	if err := cbor.Unmarshal(raw, &name); err != nil {
		return "", false
	}
	return name, true
}
