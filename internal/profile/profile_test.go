package profile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lepe/remmote/internal/api"
)

func TestStoreRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Profile{
		Name:   "Workstation",
		Server: "host:7677",
		Spec: api.SessionSpec{
			Source: api.SourceDesktop,
			Display: api.DisplaySpec{Kind: api.KindCreate,
				Create: &api.CreateSpec{Server: "xvfb", Size: "1280x800"}},
			Stream: api.StreamSpec{Codec: "hybrid", Quality: 80},
		},
		Viewer: ViewerPrefs{Upscale: 2},
	}
	if err := s.Put(p); err != nil {
		t.Fatal(err)
	}
	back, err := s.Get("Workstation")
	if err != nil {
		t.Fatal(err)
	}
	if back.Server != "host:7677" || back.Spec.Stream.Quality != 80 {
		t.Fatalf("came back as %+v", back)
	}
	if back.Spec.Display.Create == nil || back.Spec.Display.Create.Size != "1280x800" {
		t.Fatalf("create options lost: %+v", back.Spec.Display)
	}
	if back.Viewer.Upscale != 2 {
		t.Fatalf("viewer prefs lost: %+v", back.Viewer)
	}

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "Workstation" {
		t.Fatalf("list = %+v", list)
	}

	// Profiles are files a person can read.
	raw, err := os.ReadFile(filepath.Join(s.Dir(), "Workstation.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[0] != '{' {
		t.Fatalf("profile file is not JSON: %q", raw)
	}

	if err := s.Delete("Workstation"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("Workstation"); err == nil {
		t.Fatal("a deleted profile came back")
	}
}

func TestStoreKeepsNamesSafe(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Profile{Name: "mum's machine / laptop", Server: "host:7677"}
	if err := s.Put(p); err != nil {
		t.Fatal(err)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "mum's machine / laptop" {
		t.Fatalf("list = %+v", list)
	}
	// The name round-trips even though the file name had to change.
	if _, err := s.Get("mum's machine / laptop"); err != nil {
		t.Fatal(err)
	}
}

func TestStoreNeedsAName(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(Profile{Server: "host"}); err == nil {
		t.Fatal("an unnamed profile was accepted")
	}
}
