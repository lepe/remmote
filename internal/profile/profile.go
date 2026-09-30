// Package profile is the client's speed dial: the connections it knows
// how to make, named and saved so connecting is one choice rather than a
// form to fill in again.
//
// A profile is a *SessionSpec — the same JSON the control API takes —
// plus where to reach the daemon and how to identify this device. Saving
// one is therefore just saving what the daemon would be told.
package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/auth"
)

// Profile is one saved connection.
type Profile struct {
	// Name is how the connection is listed and addressed ("Workstation").
	Name string `json:"name"`
	// Server is where the daemon listens (host:port).
	Server string `json:"server"`
	// Identity is the paired device to connect as ("" = none: -tls only).
	Identity string `json:"identity,omitempty"`
	// TLS is the -tls value for an unpaired link: "auto", "SHA256:…", or
	// a shared secret. Ignored when Identity is set.
	TLS string `json:"tls,omitempty"`
	// Spec is what to share — the daemon's SessionSpec.
	Spec api.SessionSpec `json:"spec"`
	// Viewer is how to watch it on this machine.
	Viewer ViewerPrefs `json:"viewer"`
	// Updated is when the profile last changed.
	Updated time.Time `json:"updated"`
}

// ViewerPrefs are the viewing-side options: they belong to this screen,
// not to the host.
type ViewerPrefs struct {
	Upscale   int  `json:"upscale,omitempty"`   // 1, 2 or 4
	FastScale bool `json:"fastScale,omitempty"` // nearest-neighbour scaling
	Quality   int  `json:"quality,omitempty"`   // 0 = the host's default
}

// Store is a directory of profiles, one file each: hand-editable, and
// one file to copy to another machine.
type Store struct {
	dir string
}

// Open opens (and makes) the profile store. An empty directory uses the
// usual one: <config>/profiles.d.
func Open(dir string) (*Store, error) {
	if dir == "" {
		dir = filepath.Join(auth.ConfigHome(), "profiles.d")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir is where this store keeps its files.
func (s *Store) Dir() string { return s.dir }

// List returns the profiles, by name.
func (s *Store) List() ([]Profile, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(matches))
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue // a half-written file is not a broken list
		}
		var p Profile
		if json.Unmarshal(raw, &p) == nil && p.Name != "" {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// Get reads one profile.
func (s *Store) Get(name string) (Profile, error) {
	var p Profile
	raw, err := os.ReadFile(s.path(name))
	if err != nil {
		if os.IsNotExist(err) {
			return p, fmt.Errorf("profile: no connection named %q", name)
		}
		return p, err
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("profile: %s: %w", name, err)
	}
	return p, nil
}

// Put saves a profile (atomic: a crash leaves the old one, not half a
// new one).
func (s *Store) Put(p Profile) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return fmt.Errorf("profile: the connection needs a name")
	}
	p.Updated = time.Now()
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := s.path(p.Name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Delete forgets a profile.
func (s *Store) Delete(name string) error {
	err := os.Remove(s.path(name))
	if os.IsNotExist(err) {
		return fmt.Errorf("profile: no connection named %q", name)
	}
	return err
}

// path is the file one profile lives in. Names are file names here, so
// the separators a file would not survive are simply not allowed.
func (s *Store) path(name string) string {
	safe := strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':':
			return '-'
		}
		return r
	}, strings.TrimSpace(name))
	return filepath.Join(s.dir, safe+".json")
}
