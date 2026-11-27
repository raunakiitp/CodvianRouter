package webexec

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// SessionStore stores captured browser session states per catalog ID,
// encrypted at rest. It follows the sealing conventions of
// internal/server/oauth_conn.go exactly:
//
//   - when a Vault is configured, the serialized SessionState is sealed
//     with vault.Seal (Argon2id-derived key, AES-GCM, data-dir bound)
//     and only the ciphertext is held in memory or persisted;
//   - with no vault (no data dir configured) the state lives in memory
//     only, plaintext, and is never written to disk;
//   - status/list output carries NO session material — only the
//     catalog ID, whether a state exists, and timestamps.
//
// Persistence: with a vault, the SEALED blob is also persisted under
// <data-dir>/webexec-sessions/<catalog-id>.json (0600, atomic rename)
// so a captured session survives a router restart — a cookie capture
// is expensive to redo and the ciphertext is useless without the
// data-dir-bound key. Without a vault nothing is ever written to disk.
//
// The store is safe for concurrent use.
type SessionStore struct {
	mu       sync.Mutex
	vault    *crypto.Vault // nil = in-memory only
	dir      string        // "" = memory only
	sessions map[string]*storedSession
}

// storedSession is one stored state. Data holds either the sealed
// ciphertext (vault) or the raw JSON (no vault), matching the
// oauthConn AccessToken convention. persistedSession is the on-disk
// form (Data serialized only there — never in status output).
type storedSession struct {
	CatalogID  string    `json:"-"`
	Data       string    `json:"-"`
	CapturedAt time.Time `json:"captured_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type persistedSession struct {
	CatalogID  string    `json:"catalog_id"`
	Data       string    `json:"data"`
	CapturedAt time.Time `json:"captured_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// sessionsDirName is the subdirectory of the data dir holding sealed
// session blobs.
const sessionsDirName = "webexec-sessions"

// NewSessionStore builds a store. vault may be nil (in-memory only);
// with a vault whose data dir exists, sealed blobs persist across
// restarts under <data-dir>/webexec-sessions/.
func NewSessionStore(vault *crypto.Vault) *SessionStore {
	s := &SessionStore{
		vault:    vault,
		sessions: make(map[string]*storedSession),
	}
	if vault != nil {
		if d := vault.DataDir(); d != "" {
			s.dir = filepath.Join(d, sessionsDirName)
		}
	}
	return s
}

// sessionFilePath is the on-disk path for one catalog ID's blob.
func (s *SessionStore) sessionFilePath(catalogID string) string {
	return filepath.Join(s.dir, url.PathEscape(catalogID)+".json")
}

// persistLocked writes the sealed blob atomically (tmp + rename), 0600.
// Callers hold s.mu. Best-effort persistence: a failed write is an
// error surfaced to Save (the operator must know their session did NOT
// survive), never a silent drop.
func (s *SessionStore) persistLocked(st *storedSession) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("webexec: create session dir: %w", err)
	}
	blob, err := json.Marshal(persistedSession{
		CatalogID:  st.CatalogID,
		Data:       st.Data,
		CapturedAt: st.CapturedAt,
		UpdatedAt:  st.UpdatedAt,
	})
	if err != nil {
		return fmt.Errorf("webexec: serialize persisted session: %w", err)
	}
	tmp := s.sessionFilePath(st.CatalogID) + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("webexec: write session blob: %w", err)
	}
	if err := os.Rename(tmp, s.sessionFilePath(st.CatalogID)); err != nil {
		return fmt.Errorf("webexec: rename session blob: %w", err)
	}
	return nil
}

// unpersistLocked removes the on-disk blob (best effort — a failed
// delete must not block the in-memory delete).
func (s *SessionStore) unpersistLocked(catalogID string) {
	if s.dir == "" {
		return
	}
	_ = os.Remove(s.sessionFilePath(catalogID))
}

// loadPersistedLocked loads one blob from disk into memory. Callers
// hold s.mu. Returns (found, error).
func (s *SessionStore) loadPersistedLocked(catalogID string) (bool, error) {
	if s.dir == "" {
		return false, nil
	}
	blob, err := os.ReadFile(s.sessionFilePath(catalogID))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("webexec: read session blob: %w", err)
	}
	var p persistedSession
	if err := json.Unmarshal(blob, &p); err != nil {
		return false, fmt.Errorf("webexec: decode session blob: %w", err)
	}
	s.sessions[p.CatalogID] = &storedSession{
		CatalogID:  p.CatalogID,
		Data:       p.Data,
		CapturedAt: p.CapturedAt,
		UpdatedAt:  p.UpdatedAt,
	}
	return true, nil
}

// SessionStatus is the client-visible view of a stored session (no
// secrets — same rule as oauthConnStatus).
type SessionStatus struct {
	CatalogID  string    `json:"catalog_id"`
	HasSession bool      `json:"has_session"`
	CapturedAt time.Time `json:"captured_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	CookieNum  int       `json:"cookie_count"` // count only, never values
}

// Save stores (or replaces) the session state for a catalog ID. The
// state is validated first and the whole serialized state is sealed
// when a vault is configured. It refuses unknown catalog IDs: a session
// for something that is not one of the 37 WebCookie entries can never
// be routed, so storing it would be a silent trap.
func (s *SessionStore) Save(catalogID string, state *SessionState) error {
	if _, ok := surfaceByID(catalogID); !ok {
		return fmt.Errorf("webexec: %w: %q", ErrUnknownSurface, catalogID)
	}
	if err := state.Validate(); err != nil {
		return err
	}
	plain, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("webexec: serialize session state: %w", err)
	}
	data := string(plain)
	if s.vault != nil {
		sealed, err := s.vault.Seal(plain)
		if err != nil {
			return fmt.Errorf("webexec: encrypt session state: %w", err)
		}
		data = sealed
	}
	now := time.Now()
	capturedAt := state.CapturedAt
	if capturedAt.IsZero() {
		capturedAt = now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[catalogID] = &storedSession{
		CatalogID:  catalogID,
		Data:       data,
		CapturedAt: capturedAt,
		UpdatedAt:  now,
	}
	if err := s.persistLocked(s.sessions[catalogID]); err != nil {
		// Roll the in-memory entry back out: the operator must not be
		// left believing a session survived when its blob did not.
		delete(s.sessions, catalogID)
		return err
	}
	return nil
}

// Load returns the stored session state for a catalog ID, decrypting
// via the Vault when configured.
//
// Stale inputs are refused, not served: a corrupt or unsealable blob,
// or a state older than maxSessionAge, wraps ErrStaleSession so the
// executor answers 401 semantics instead of replaying a dead session.
func (s *SessionStore) Load(catalogID string) (*SessionState, error) {
	s.mu.Lock()
	st, ok := s.sessions[catalogID]
	if !ok {
		found, err := s.loadPersistedLocked(catalogID)
		if err != nil {
			s.mu.Unlock()
			return nil, fmt.Errorf("webexec: %w: stored state for %q unreadable: %v", ErrStaleSession, catalogID, err)
		}
		if found {
			st = s.sessions[catalogID]
		}
	}
	s.mu.Unlock()
	if st == nil {
		return nil, fmt.Errorf("webexec: %w: %q", ErrNoSession, catalogID)
	}
	plain := []byte(st.Data)
	if s.vault != nil {
		var err error
		plain, err = s.vault.Unseal(st.Data)
		if err != nil {
			return nil, fmt.Errorf("webexec: %w: decrypt stored state for %q: %v", ErrStaleSession, catalogID, err)
		}
	}
	var state SessionState
	if err := json.Unmarshal(plain, &state); err != nil {
		return nil, fmt.Errorf("webexec: %w: decode stored state for %q: %v", ErrStaleSession, catalogID, err)
	}
	if err := state.Validate(); err != nil {
		return nil, fmt.Errorf("webexec: %w: stored state for %q: %v", ErrStaleSession, catalogID, err)
	}
	if state.IsStale(time.Now()) {
		return nil, fmt.Errorf("webexec: %w: stored state for %q captured at %s is older than the %d-day bound — re-capture", ErrStaleSession, catalogID, state.CapturedAt.Format(time.RFC3339), int(maxSessionAge.Hours()/24))
	}
	return &state, nil
}

// Delete removes the stored state for a catalog ID (memory and, with a
// vault, the persisted sealed blob).
func (s *SessionStore) Delete(catalogID string) error {
	s.mu.Lock()
	_, ok := s.sessions[catalogID]
	if !ok {
		var err error
		ok, err = s.loadPersistedLocked(catalogID)
		if err != nil {
			s.mu.Unlock()
			return err
		}
	}
	if ok {
		delete(s.sessions, catalogID)
		s.unpersistLocked(catalogID)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("webexec: no stored session for %q", catalogID)
	}
	return nil
}

// Has reports whether a state is stored for a catalog ID (memory or
// persisted blob).
func (s *SessionStore) Has(catalogID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[catalogID]; ok {
		return true
	}
	found, err := s.loadPersistedLocked(catalogID)
	return err == nil && found
}

// List returns status for every stored session (no session material).
func (s *SessionStore) List() []SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]SessionStatus, 0, len(ids))
	for _, id := range ids {
		st := s.sessions[id]
		cookieNum := 0
		if plain := st.Data; plain != "" {
			// With a vault the blob is ciphertext; decrypt a copy
			// just for the count, best effort — a count is not a
			// secret, but a decrypt failure must not fail List.
			var state SessionState
			data := []byte(plain)
			if s.vault != nil {
				if p, err := s.vault.Unseal(plain); err == nil {
					data = p
				}
			}
			if err := json.Unmarshal(data, &state); err == nil {
				cookieNum = len(state.Cookies)
			}
		}
		out = append(out, SessionStatus{
			CatalogID:  id,
			HasSession: true,
			CapturedAt: st.CapturedAt,
			UpdatedAt:  st.UpdatedAt,
			CookieNum:  cookieNum,
		})
	}
	return out
}
