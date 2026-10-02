package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lepe/remmote/internal/api"
)

func TestParseAllowList(t *testing.T) {
	data := []byte("# the tools the demo needs\nxcalc\n\n  xclock  \n# disabled: gimp\n*\n")
	got := ParseAllowList(data)
	want := []string{"xcalc", "xclock", "*"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parsed %q, want %q", got, want)
	}
	if ParseAllowList(nil) != nil {
		t.Fatal("an empty list is nil, not a one-empty-string list")
	}
}

func TestLoadAllowList(t *testing.T) {
	if _, err := LoadAllowList(t.TempDir() + "/absent.txt"); err == nil {
		t.Fatal("a missing file must be an error, not an empty allow list")
	}
}

// The host info is what a client's editor fills its choices from: what
// may be launched, and by name.
func TestHostSaysWhatMayBeLaunched(t *testing.T) {
	d, err := New(Options{AllowExec: []string{"xcalc", "xclock"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(d.mux())
	defer srv.Close()

	resp, err := http.Get(srv.URL + api.PathHost)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var h api.HostInfo
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if !h.AllowExec {
		t.Fatal("a daemon with -allow-exec reported none")
	}
	if strings.Join(h.AllowExecCommands, ",") != "xcalc,xclock" {
		t.Fatalf("the host's answer named %q", h.AllowExecCommands)
	}
}
