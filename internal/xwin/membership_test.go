package xwin

import (
	"testing"

	"github.com/jezek/xgb/xproto"
)

func TestParseWMClass(t *testing.T) {
	cases := []struct {
		in              []byte
		instance, class string
	}{
		{[]byte{'x', 'c', 'a', 'l', 'c', 0, 'X', 'C', 'a', 'l', 'c', 0}, "xcalc", "XCalc"},
		{[]byte("only\x00"), "only", ""},
		{[]byte("a\x00b\x00c\x00"), "a", "b"},
		{[]byte{}, "", ""},
	}
	for _, tc := range cases {
		i, c := ParseWMClass(tc.in)
		if i != tc.instance || c != tc.class {
			t.Errorf("ParseWMClass(%q) = (%q,%q), want (%q,%q)", tc.in, i, c, tc.instance, tc.class)
		}
	}
}

func TestPredicatesAccept(t *testing.T) {
	tracked := map[xproto.Window]bool{10: true}
	p := Predicates{
		SeedClass: "XCalc",
		PID:       4242,
		Tracked:   func(w xproto.Window) bool { return tracked[w] },
	}
	cases := []struct {
		name      string
		transient xproto.Window
		pid       uint32
		class     string
		want      bool
	}{
		{"transient tracked", 10, 0, "", true},
		{"transient untracked", 99, 0, "", false},
		{"pid match", 0, 4242, "", true},
		{"pid mismatch", 0, 1, "", false},
		{"pid zero", 0, 0, "", false},
		{"class match exact", 0, 0, "XCalc", true},
		{"class match case", 0, 0, "xcalc", true},
		{"class mismatch", 0, 0, "Firefox", false},
		{"class empty seed", 0, 0, "", false},
		{"any", 10, 4242, "XCalc", true},
	}
	for _, tc := range cases {
		if got := p.Accept(tc.transient, tc.pid, tc.class); got != tc.want {
			t.Errorf("%s: Accept(%d,%d,%q) = %v, want %v", tc.name, tc.transient, tc.pid, tc.class, got, tc.want)
		}
	}

	// Empty predicates never accept.
	empty := Predicates{}
	if empty.Accept(10, 4242, "XCalc") {
		t.Error("empty predicates accepted a window")
	}
}
