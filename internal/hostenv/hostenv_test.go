package hostenv

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// The Xauthority file has to be exactly what X11/Xauth.h describes:
// family u16 followed by four u16-length-prefixed fields, big-endian. A
// malformed one is not an error anyone sees until a connection is
// refused, so it is worth reading back here.
func TestWriteAuthorityRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth")
	if err := writeAuthority(path, "88"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("cookie file mode %v, want 0600", st.Mode().Perm())
	}

	family := binary.BigEndian.Uint16(raw)
	if family != 0xffff {
		t.Fatalf("family %#x, want FamilyWild (0xffff)", family)
	}
	off := 2
	field := func() string {
		n := int(binary.BigEndian.Uint16(raw[off:]))
		off += 2
		s := string(raw[off : off+n])
		off += n
		return s
	}
	if addr := field(); addr != "" {
		t.Fatalf("address %q, want empty (FamilyWild matches any)", addr)
	}
	if num := field(); num != "88" {
		t.Fatalf("display number %q, want \"88\"", num)
	}
	if name := field(); name != "MIT-MAGIC-COOKIE-1" {
		t.Fatalf("protocol %q, want MIT-MAGIC-COOKIE-1", name)
	}
	n := int(binary.BigEndian.Uint16(raw[off:]))
	off += 2
	if n != 16 {
		t.Fatalf("cookie length %d, want 16", n)
	}
	if off+n != len(raw) {
		t.Fatalf("%d trailing bytes after the cookie", len(raw)-off-n)
	}
}

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{
		"":         "",
		"88":       ":88",
		":88":      ":88",
		"unix:88":  ":88",
		":88.0":    ":88",
		"  :99  ":  ":99",
		"nonsense": "",
		":":        "",
	} {
		if got := normalizeName(in); got != want {
			t.Errorf("normalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetEnvReplacesAndKeepsOrder(t *testing.T) {
	env := setEnv([]string{"A=1", "DISPLAY=:old", "B=2"}, "DISPLAY", ":new")
	if len(env) != 3 || env[0] != "A=1" || env[1] != "B=2" || env[2] != "DISPLAY=:new" {
		t.Fatalf("setEnv = %v", env)
	}
	if env = setEnv([]string{"A=1"}, "XAUTHORITY", "/tmp/x"); len(env) != 2 || env[1] != "XAUTHORITY=/tmp/x" {
		t.Fatalf("setEnv append = %v", env)
	}
}

func TestIsSize(t *testing.T) {
	for s, want := range map[string]bool{
		"1280x800": true, "640x480": true, "160x160": true,
		"": false, "big": false, "1280x": false, "100x100": false, "800x600x24": false,
	} {
		if got := isSize(s); got != want {
			t.Errorf("isSize(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestNoneWords(t *testing.T) {
	for _, w := range []string{"", "none", "None", "NO", "off", "-"} {
		if !isNoneWord(w) {
			t.Errorf("isNoneWord(%q) = false", w)
		}
	}
	if isNoneWord("openbox") {
		t.Error("isNoneWord(openbox) = true")
	}
}
