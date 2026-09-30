package hub

import (
	"testing"

	"github.com/lepe/remmote/internal/api"
	"github.com/lepe/remmote/internal/profile"
)

// The editor edits text; the profile is built from it. The two have to
// agree, or a connection saved from the form is not the one the form
// showed.
func TestDraftRoundTrip(t *testing.T) {
	in := profile.Profile{
		Name:     "Workstation",
		Server:   "host:7677",
		Identity: "laptop",
		Spec: api.SessionSpec{
			Source:   api.SourceApp,
			Maximize: true,
			Display: api.DisplaySpec{Kind: api.KindCreate,
				Create: &api.CreateSpec{Server: "xephyr", Size: "1280x800",
					WM: "openbox", HostDisplay: ":0"}},
			App:    &api.AppSpec{Command: "xcalc -big"},
			Stream: api.StreamSpec{Codec: "jpeg", Quality: 80, FPS: 30, Downscale: 2},
		},
		Viewer: profile.ViewerPrefs{Upscale: 2, FastScale: true},
	}
	out := draftOf(in).profile()
	if out.Name != in.Name || out.Server != in.Server || out.Identity != in.Identity {
		t.Fatalf("identity lost: %+v", out)
	}
	if out.Spec.Source != api.SourceApp || !out.Spec.Maximize {
		t.Fatalf("source lost: %+v", out.Spec)
	}
	if out.Spec.App == nil || out.Spec.App.Command != "xcalc -big" {
		t.Fatalf("app lost: %+v", out.Spec.App)
	}
	if out.Spec.Display.Kind != api.KindCreate || out.Spec.Display.Create == nil {
		t.Fatalf("display lost: %+v", out.Spec.Display)
	}
	c := out.Spec.Display.Create
	if c.Server != "xephyr" || c.Size != "1280x800" || c.WM != "openbox" || c.HostDisplay != ":0" {
		t.Fatalf("create options lost: %+v", c)
	}
	s := out.Spec.Stream
	if s.Codec != "jpeg" || s.Quality != 80 || s.FPS != 30 || s.Downscale != 2 {
		t.Fatalf("stream options lost: %+v", s)
	}
	if out.Viewer.Upscale != 2 || !out.Viewer.FastScale {
		t.Fatalf("viewer prefs lost: %+v", out.Viewer)
	}
}

// An empty draft is still a valid starting point: what the form shows
// when "New" is pressed.
func TestDraftDefaults(t *testing.T) {
	p := draftOf(profile.Profile{}).profile()
	if p.Spec.Source != api.SourceDesktop {
		t.Fatalf("source = %q, want desktop", p.Spec.Source)
	}
	if p.Spec.Display.Kind != api.KindExisting {
		t.Fatalf("display kind = %q, want existing", p.Spec.Display.Kind)
	}
	if p.Spec.Stream.Clipboard == nil || !*p.Spec.Stream.Clipboard {
		t.Fatal("clipboard should default to on")
	}
}
