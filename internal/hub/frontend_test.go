package hub

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestFrontend drives the page's own app.js against a small DOM, so the
// parts that decide what the interface shows are checked here rather
// than by looking at it: which panel is up, what a row does when it is
// clicked, and what the form sends back. The interface has been wrong in
// ways screenshots agreed with — a class and an inline style disagreeing
// about whether a panel was visible — and this is the check that names
// the one at fault.
//
// It needs node; without it the test says so and skips.
func TestFrontend(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; the interface is not being checked")
	}
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not find this test's own directory")
	}
	app := filepath.Join(filepath.Dir(thisFile), "frontend", "app.js")

	cmd := exec.Command(node, filepath.Join(filepath.Dir(thisFile), "testdata", "domtest.mjs"), app)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("the interface's own logic failed: %v", err)
	}
}
