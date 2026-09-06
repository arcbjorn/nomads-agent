// Package storage persists the session and configuration locally.
//
// State lives in a single JSON file under the user's config directory with
// owner-only permissions. It holds session cookies, so it is treated as a
// credential file throughout: 0600 on the file, 0700 on the directory, and
// never logged.
package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// FileMode is owner read/write only — this file holds session cookies.
	FileMode = 0o600
	// DirMode is owner-only access to the state directory.
	DirMode = 0o700
)

// Cookie is a persisted HTTP cookie.
type Cookie struct {
	Name     string    `json:"name"`
	Value    string    `json:"value"`
	Domain   string    `json:"domain,omitempty"`
	Path     string    `json:"path,omitempty"`
	Expires  time.Time `json:"expires,omitempty"`
	Secure   bool      `json:"secure,omitempty"`
	HTTPOnly bool      `json:"http_only,omitempty"`
}

// Session is the authenticated state needed to talk to Nomads.com.
type Session struct {
	Username  string    `json:"username,omitempty"`
	Cookies   []Cookie  `json:"cookies,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
	// LoginHash is the magic-link secret, stored so `auth refresh` can renew a
	// session unattended. It is a permanent credential: never log or print it.
	LoginHash string `json:"login_hash,omitempty"`
	// APIKey is the personal key from https://nomads.com/settings used by the
	// documented API. Also a credential: never log or print it.
	APIKey string `json:"api_key,omitempty"`
}

// HasAPIKey reports whether the documented API can be used.
func (s *Session) HasAPIKey() bool { return s != nil && s.APIKey != "" && s.Username != "" }

// HasBrowserSession reports whether the first-party endpoints can be used.
func (s *Session) HasBrowserSession() bool { return s != nil && len(s.Cookies) > 0 }

// IsEmpty reports whether there is no usable credential of any kind.
func (s *Session) IsEmpty() bool {
	return s == nil || (len(s.Cookies) == 0 && s.APIKey == "")
}

// State is the whole persisted file.
type State struct {
	Session *Session `json:"session,omitempty"`
}

// Store reads and writes the state file.
type Store struct{ path string }

// DefaultDir is the platform config directory for this tool.
func DefaultDir() (string, error) {
	if d := os.Getenv("NOMADS_AGENT_HOME"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "darwin" {
		// Prefer XDG-style ~/.config on macOS too: it matches the documented
		// path and keeps Linux/macOS setups identical.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "nomads-agent"), nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "nomads-agent"), nil
}

// New opens the store at dir, or the default directory when dir is empty.
func New(dir string) (*Store, error) {
	if dir == "" {
		var err error
		if dir, err = DefaultDir(); err != nil {
			return nil, fmt.Errorf("resolve config dir: %w", err)
		}
	}
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}
	// MkdirAll leaves an existing directory's permissions alone, so tighten
	// them explicitly: this directory holds session cookies and an API key.
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, DirMode); err != nil {
			return nil, fmt.Errorf("secure config dir: %w", err)
		}
	}
	return &Store{path: filepath.Join(dir, "state.json")}, nil
}

// Path is the state file location, safe to print.
func (s *Store) Path() string { return s.path }

// Load reads the state, returning an empty state when the file is absent.
func (s *Store) Load() (*State, error) {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", s.path, err)
	}
	return &st, nil
}

// Save writes the state atomically with owner-only permissions.
func (s *Store) Save(st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	// Write to a temp file in the same directory, then rename, so a crash or a
	// concurrent reader never observes a half-written credential file.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(FileMode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp state: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("install state file: %w", err)
	}
	return nil
}

// LoadSession returns the stored session, or an empty one when there is none.
//
// It never returns nil alongside a nil error: callers routinely inspect the
// result's fields, and an empty session is the correct representation of
// "nothing configured yet".
func (s *Store) LoadSession() (*Session, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	if st.Session == nil {
		return &Session{}, nil
	}
	return st.Session, nil
}

// SaveSession replaces the stored session, stamping the update time.
func (s *Store) SaveSession(sess *Session) error {
	st, err := s.Load()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if sess != nil {
		if sess.CreatedAt.IsZero() {
			sess.CreatedAt = now
		}
		sess.UpdatedAt = now
	}
	st.Session = sess
	return s.Save(st)
}

// Clear removes the stored session and deletes the state file.
func (s *Store) Clear() error {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove state: %w", err)
	}
	return nil
}
