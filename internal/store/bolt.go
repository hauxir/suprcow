package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/hauxir/suprcow/internal/env"
	bolt "go.etcd.io/bbolt"
)

var bucketName = []byte("environments")

// Bolt is a durable Store backed by a bbolt (pure-Go) file. One bucket holds
// JSON-encoded environments keyed by env.Key.
type Bolt struct {
	db *bolt.DB
}

// OpenBolt opens (creating if needed) a bbolt database at path.
func OpenBolt(path string) (*Bolt, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucketName)
		return e
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init bucket: %w", err)
	}
	return &Bolt{db: db}, nil
}

func (s *Bolt) Get(project string, pr int) (*env.Environment, bool, error) {
	var e env.Environment
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketName).Get([]byte(env.Key(project, pr)))
		if raw == nil {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &e)
	})
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	return &e, true, nil
}

func (s *Bolt) Put(e *env.Environment) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte(e.Key()), raw)
	})
}

func (s *Bolt) Delete(project string, pr int) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Delete([]byte(env.Key(project, pr)))
	})
}

func (s *Bolt) List() ([]*env.Environment, error) {
	var out []*env.Environment
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).ForEach(func(_, v []byte) error {
			var e env.Environment
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, &e)
			return nil
		})
	})
	return out, err
}

func (s *Bolt) Close() error { return s.db.Close() }
