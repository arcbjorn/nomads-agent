package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess := &Session{
		Username:  "testuser",
		Cookies:   []Cookie{{Name: "PHPSESSID", Value: "abc", Path: "/"}},
		APIKey:    "testkey",
		LoginHash: "testhash",
	}
	if err := s.SaveSession(sess); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadSession()
	if err != nil {
		t.Fatal(err)
	}
	if got.Username != "testuser" || got.APIKey != "testkey" || len(got.Cookies) != 1 {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.UpdatedAt.IsZero() || got.CreatedAt.IsZero() {
		t.Error("timestamps were not stamped")
	}
}

// The state file holds session cookies and an API key, so its permissions are
// a security control, not a detail.
func TestStateFileIsOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(&Session{Username: "u", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != FileMode {
		t.Fatalf("state file is %o, want %o (owner read/write only)", perm, FileMode)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config dir is group/world accessible: %o", perm)
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.Load()
	if err != nil {
		t.Fatalf("a missing state file should load as empty: %v", err)
	}
	if st.Session != nil {
		t.Error("expected no session")
	}
}

// A save must not leave a half-written credential file behind.
func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := s.SaveSession(&Session{Username: "u", APIKey: "k"}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
	}
}

func TestClearRemovesState(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSession(&Session{Username: "u", APIKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Error("state file should be gone")
	}
	// Clearing twice must be safe.
	if err := s.Clear(); err != nil {
		t.Errorf("second clear should be a no-op: %v", err)
	}
}

func TestSessionCapabilityFlags(t *testing.T) {
	cases := []struct {
		name           string
		sess           *Session
		empty          bool
		hasKey, hasWeb bool
	}{
		{"nothing", &Session{}, true, false, false},
		{"key only", &Session{Username: "u", APIKey: "k"}, false, true, false},
		{"cookies only", &Session{Cookies: []Cookie{{Name: "a"}}}, false, false, true},
		{"key without username", &Session{APIKey: "k"}, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sess.IsEmpty(); got != c.empty {
				t.Errorf("IsEmpty = %v, want %v", got, c.empty)
			}
			if got := c.sess.HasAPIKey(); got != c.hasKey {
				t.Errorf("HasAPIKey = %v, want %v", got, c.hasKey)
			}
			if got := c.sess.HasBrowserSession(); got != c.hasWeb {
				t.Errorf("HasBrowserSession = %v, want %v", got, c.hasWeb)
			}
		})
	}
}

// LoadSession must never hand back a nil pointer next to a nil error: callers
// read its fields directly, and a fresh install has no state file at all.
func TestLoadSessionNeverReturnsNil(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.LoadSession()
	if err != nil {
		t.Fatal(err)
	}
	if sess == nil {
		t.Fatal("LoadSession returned nil for a fresh config dir")
	}
	// The zero session must answer its capability questions safely.
	if !sess.IsEmpty() || sess.HasAPIKey() || sess.HasBrowserSession() {
		t.Errorf("unexpected flags on an empty session: %+v", sess)
	}
}
