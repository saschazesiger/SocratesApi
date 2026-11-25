package store

import (
	"errors"

	zstore "github.com/saschazesiger/Z-Dash/server/store"
)

// ServerKV is the kv table as the Z-Dash server keeps its own state in it:
// the admin password (auth.password), the sessions (auth.sessions) and the
// tunnel (cloudflare:*). The password and the sessions are the keys and the
// JSON Socrates API wrote before it ran on the server, so an update keeps
// both.
type ServerKV struct{ St *Store }

// Get reads a key; the server's ErrNotFound when it is not there.
func (k ServerKV) Get(key string) ([]byte, error) {
	v, err := k.St.Get(key)
	if errors.Is(err, ErrNotFound) {
		return nil, zstore.ErrNotFound
	}
	return v, err
}

// Set writes a key.
func (k ServerKV) Set(key string, value []byte) error { return k.St.Set(key, value) }

// SetIfMissing writes a key only when it is not there, in one statement.
func (k ServerKV) SetIfMissing(key string, value []byte) (bool, error) {
	return k.St.SetIfMissing(key, value)
}

// Delete removes a key; a missing key is no error.
func (k ServerKV) Delete(key string) error { return k.St.Delete(key) }
