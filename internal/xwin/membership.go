package xwin

import (
	"strings"

	"github.com/jezek/xgb/xproto"
)

// Predicates decides whether a window belongs to the shared application.
// All fields are optional; any single match accepts.
type Predicates struct {
	SeedClass string                   // res_class of the seed window ("" unknown)
	PID       uint32                   // child process id (0 unknown)
	Tracked   func(xproto.Window) bool // current tracked-set membership
}

// Accept reports whether the window belongs: it is transient for a
// tracked window (dialogs, menus), or was created by the child process
// (_NET_WM_PID), or shares the seed window's WM_CLASS.
func (p Predicates) Accept(transientFor xproto.Window, pid uint32, class string) bool {
	if p.Tracked != nil && transientFor != 0 && p.Tracked(transientFor) {
		return true
	}
	if p.PID != 0 && pid != 0 && pid == p.PID {
		return true
	}
	if p.SeedClass != "" && class != "" && strings.EqualFold(class, p.SeedClass) {
		return true
	}
	return false
}

// ParseWMClass splits a WM_CLASS property value ("instance\0class\0").
// Anything after the second NUL is ignored.
func ParseWMClass(data []byte) (instance, class string) {
	s := string(data)
	cut := func(v string) string {
		if i := strings.IndexByte(v, 0); i >= 0 {
			return v[:i]
		}
		return v
	}
	parts := strings.SplitN(s, "\x00", 2)
	instance = cut(parts[0])
	if len(parts) > 1 {
		class = cut(parts[1])
	}
	return instance, class
}
