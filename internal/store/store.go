package store

import (
	"errors"
	"path/filepath"

	"github.com/dgraph-io/badger/v4"
)

type Store struct {
	db *badger.DB
}

func Open(dataDir string) (*Store, error) {
	opts := badger.DefaultOptions(filepath.Join(dataDir, "db")).
		WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Put(key, value []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Set(key, value)
	})
}

func (s *Store) Get(key []byte) ([]byte, error) {
	var out []byte
	err := s.db.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return err
		}
		out, err = item.ValueCopy(nil)
		return err
	})
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, ErrNotFound
	}
	return out, err
}

func (s *Store) Delete(key []byte) error {
	return s.db.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
}

// Iterate calls fn for every key/value pair whose key starts with prefix.
// Returning false from fn stops iteration.
func (s *Store) Iterate(prefix []byte, fn func(k, v []byte) bool) error {
	return s.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			item := it.Item()
			v, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			if !fn(item.KeyCopy(nil), v) {
				return nil
			}
		}
		return nil
	})
}

var ErrNotFound = errors.New("store: key not found")
