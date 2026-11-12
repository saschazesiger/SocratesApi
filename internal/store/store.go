// Package store keeps all state in one SQLite file under the data
// directory: settings, the admin password, models, API keys and a short
// request log. It is the only place that knows SQL.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store is the database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS models (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, repo TEXT NOT NULL DEFAULT '', revision TEXT NOT NULL DEFAULT 'main',
  files TEXT NOT NULL DEFAULT '[]', mmproj TEXT NOT NULL DEFAULT '', quant TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, error TEXT NOT NULL DEFAULT '',
  settings TEXT NOT NULL DEFAULT '{}', meta TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
  id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, prefix TEXT NOT NULL, hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL DEFAULT 0, requests INTEGER NOT NULL DEFAULT 0,
  tokens_in INTEGER NOT NULL DEFAULT 0, tokens_out INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS requests (
  id INTEGER PRIMARY KEY AUTOINCREMENT, at INTEGER NOT NULL, source TEXT NOT NULL, key_name TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', status INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0, cached_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0,
  prompt_tps REAL NOT NULL DEFAULT 0, gen_tps REAL NOT NULL DEFAULT 0, ms INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT ''
);
`

// Open opens (and creates) the database in dir; an empty dir gives an
// in-memory database for tests.
func Open(dir string) (*Store, error) {
	dsn := "file::memory:"
	if dir != "" {
		dsn = "file:" + filepath.Join(dir, "socrates.db") +
			"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Now is the current time in Unix milliseconds.
func Now() int64 { return time.Now().UnixMilli() }

// --- key/value ---------------------------------------------------------------

// GetJSON reads a value into v; ErrNotFound if the key is not set.
func (s *Store) GetJSON(key string, v any) error {
	var b []byte
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// SetJSON stores v under key.
func (s *Store) SetJSON(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO kv(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, b)
	return err
}

// SetJSONIfMissing stores v only if key is not set; it reports whether it did.
func (s *Store) SetJSONIfMissing(key string, v any) (bool, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(`INSERT INTO kv(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, key, b)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Delete removes a key.
func (s *Store) Delete(key string) error {
	_, err := s.db.Exec(`DELETE FROM kv WHERE key = ?`, key)
	return err
}

// --- models ------------------------------------------------------------------

// File is one file of a model on disk, relative to the model's folder.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size"`
}

// Model is a model the user added.
type Model struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Repo      string          `json:"repo"`
	Revision  string          `json:"revision"`
	Files     []File          `json:"files"`  // the GGUF file(s); split files in order
	MMProj    string          `json:"mmproj"` // vision projector, relative path, optional
	Quant     string          `json:"quant"`
	Size      int64           `json:"size"`
	State     string          `json:"state"` // queued, downloading, converting, ready, failed
	Error     string          `json:"error"`
	Settings  json.RawMessage `json:"settings"`
	Meta      json.RawMessage `json:"meta"`
	CreatedAt int64           `json:"createdAt"`
	UpdatedAt int64           `json:"updatedAt"`
}

const modelCols = `id, name, repo, revision, files, mmproj, quant, size, state, error, settings, meta, created_at, updated_at`

func scanModel(sc interface{ Scan(...any) error }) (Model, error) {
	var m Model
	var files, settings, meta string
	err := sc.Scan(&m.ID, &m.Name, &m.Repo, &m.Revision, &files, &m.MMProj, &m.Quant, &m.Size, &m.State, &m.Error,
		&settings, &meta, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return m, err
	}
	_ = json.Unmarshal([]byte(files), &m.Files)
	m.Settings = json.RawMessage(settings)
	m.Meta = json.RawMessage(meta)
	return m, nil
}

// Models lists all models, oldest first.
func (s *Store) Models() ([]Model, error) {
	rows, err := s.db.Query(`SELECT ` + modelCols + ` FROM models ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Model reads one model.
func (s *Store) Model(id string) (Model, error) {
	m, err := scanModel(s.db.QueryRow(`SELECT `+modelCols+` FROM models WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// PutModel inserts or replaces a model.
func (s *Store) PutModel(m Model) error {
	files, _ := json.Marshal(m.Files)
	if m.Files == nil {
		files = []byte("[]")
	}
	if len(m.Settings) == 0 {
		m.Settings = json.RawMessage("{}")
	}
	if len(m.Meta) == 0 {
		m.Meta = json.RawMessage("{}")
	}
	now := Now()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	_, err := s.db.Exec(`INSERT INTO models(`+modelCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, repo=excluded.repo, revision=excluded.revision, files=excluded.files,
		mmproj=excluded.mmproj, quant=excluded.quant, size=excluded.size, state=excluded.state, error=excluded.error,
		settings=excluded.settings, meta=excluded.meta, updated_at=excluded.updated_at`,
		m.ID, m.Name, m.Repo, m.Revision, string(files), m.MMProj, m.Quant, m.Size, m.State, m.Error,
		string(m.Settings), string(m.Meta), m.CreatedAt, now)
	return err
}

// SetModelState changes the state and error of a model.
func (s *Store) SetModelState(id, state, errText string) error {
	_, err := s.db.Exec(`UPDATE models SET state = ?, error = ?, updated_at = ? WHERE id = ?`, state, errText, Now(), id)
	return err
}

// DeleteModel removes a model's row.
func (s *Store) DeleteModel(id string) error {
	_, err := s.db.Exec(`DELETE FROM models WHERE id = ?`, id)
	return err
}

// --- API keys ----------------------------------------------------------------

// Key is an API key; only its hash is stored.
type Key struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"prefix"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt"`
	Requests   int64  `json:"requests"`
	TokensIn   int64  `json:"tokensIn"`
	TokensOut  int64  `json:"tokensOut"`
}

// Keys lists the API keys, newest first.
func (s *Store) Keys() ([]Key, error) {
	rows, err := s.db.Query(`SELECT id, name, prefix, created_at, last_used_at, requests, tokens_in, tokens_out FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.Requests, &k.TokensIn, &k.TokensOut); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// AddKey stores a new key by its hash.
func (s *Store) AddKey(name, prefix, hash string) (Key, error) {
	now := Now()
	res, err := s.db.Exec(`INSERT INTO api_keys(name, prefix, hash, created_at) VALUES(?,?,?,?)`, name, prefix, hash, now)
	if err != nil {
		return Key{}, err
	}
	id, _ := res.LastInsertId()
	return Key{ID: id, Name: name, Prefix: prefix, CreatedAt: now}, nil
}

// KeyByHash finds a key by the hash of its secret.
func (s *Store) KeyByHash(hash string) (Key, error) {
	var k Key
	err := s.db.QueryRow(`SELECT id, name, prefix, created_at, last_used_at, requests, tokens_in, tokens_out FROM api_keys WHERE hash = ?`, hash).
		Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.Requests, &k.TokensIn, &k.TokensOut)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	return k, err
}

// KeyCount returns how many keys exist.
func (s *Store) KeyCount() int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n)
	return n
}

// DeleteKey revokes a key.
func (s *Store) DeleteKey(id int64) error {
	res, err := s.db.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- request log -------------------------------------------------------------

// Request is one finished API or chat request.
type Request struct {
	ID               int64   `json:"id"`
	At               int64   `json:"at"`
	Source           string  `json:"source"` // api or chat
	KeyName          string  `json:"keyName"`
	KeyID            int64   `json:"-"`
	Path             string  `json:"path"`
	Model            string  `json:"model"`
	Status           int     `json:"status"`
	PromptTokens     int64   `json:"promptTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	PromptTPS        float64 `json:"promptTps"`
	GenTPS           float64 `json:"genTps"`
	Ms               int64   `json:"ms"`
	Error            string  `json:"error"`
}

// keepRequests is how many requests the log keeps.
const keepRequests = 2000

// LogRequest appends a request and updates the key's counters.
func (s *Store) LogRequest(r Request) error {
	_, err := s.db.Exec(`INSERT INTO requests(at, source, key_name, path, model, status, prompt_tokens, cached_tokens, completion_tokens, prompt_tps, gen_tps, ms, error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.At, r.Source, r.KeyName, r.Path, r.Model, r.Status, r.PromptTokens, r.CachedTokens,
		r.CompletionTokens, r.PromptTPS, r.GenTPS, r.Ms, r.Error)
	if err != nil {
		return err
	}
	if r.KeyID > 0 {
		_, err = s.db.Exec(`UPDATE api_keys SET last_used_at = ?, requests = requests + 1, tokens_in = tokens_in + ?, tokens_out = tokens_out + ? WHERE id = ?`,
			r.At, r.PromptTokens, r.CompletionTokens, r.KeyID)
		if err != nil {
			return err
		}
	}
	_, err = s.db.Exec(`DELETE FROM requests WHERE id <= (SELECT MAX(id) FROM requests) - ?`, keepRequests)
	return err
}

// Requests lists the latest requests, newest first.
func (s *Store) Requests(limit int) ([]Request, error) {
	rows, err := s.db.Query(`SELECT id, at, source, key_name, path, model, status, prompt_tokens, cached_tokens, completion_tokens, prompt_tps, gen_tps, ms, error
		FROM requests ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		var r Request
		if err := rows.Scan(&r.ID, &r.At, &r.Source, &r.KeyName, &r.Path, &r.Model, &r.Status, &r.PromptTokens, &r.CachedTokens,
			&r.CompletionTokens, &r.PromptTPS, &r.GenTPS, &r.Ms, &r.Error); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
