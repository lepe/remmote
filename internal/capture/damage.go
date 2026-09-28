package capture

import (
	"fmt"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/damage"
	"github.com/jezek/xgb/randr"
	"github.com/jezek/xgb/xproto"
)

// initDamage creates the damage object on the root window at bounding-box
// report level and subscribes to RANDR screen-change notifications. The
// XDamage spec damages the whole drawable upon creation, so the first
// frame request arrives without any screen activity.
func (c *Capturer) initDamage() error {
	// Both extensions demand a version handshake before anything else.
	if _, err := damage.QueryVersion(c.xc.X, 1, 1).Reply(); err != nil {
		return fmt.Errorf("damage query version: %w", err)
	}
	dmgID, err := c.xc.X.NewId()
	if err != nil {
		return err
	}
	if err := damage.CreateChecked(c.xc.X, damage.Damage(dmgID),
		xproto.Drawable(c.xc.Root()), damage.ReportLevelBoundingBox).Check(); err != nil {
		return err
	}
	c.dmg = damage.Damage(dmgID)
	c.hasDamage = true
	if _, err := randr.QueryVersion(c.xc.X, 1, 2).Reply(); err != nil {
		c.log.Warn("RANDR query version failed; resize notifications disabled", "err", err)
		return nil
	}
	if err := randr.SelectInputChecked(c.xc.X, c.xc.Root(), randr.NotifyMaskScreenChange).Check(); err != nil {
		// Non-fatal: we lose resize notifications, nothing else.
		c.log.Warn("RANDR select input failed; resize notifications disabled", "err", err)
	}
	return nil
}

// HandleEvent dispatches one X event. Only the pump goroutine calls this.
// It never issues requests with replies and never blocks: damage storms
// must degrade to the capture loop's pace, not stall the queue.
//
// NOTE: xgb delivers events as VALUES (their constructors return the
// struct, not a pointer), so every case matches the value type.
func (c *Capturer) HandleEvent(ev xgb.Event) {
	switch e := ev.(type) {
	case damage.NotifyEvent:
		r := clampRect(xprotoRect(e.Area), c.ScreenRect())
		c.mu.Lock()
		c.pending = unionRect(c.pending, r)
		nonEmpty := !c.pending.Empty()
		c.mu.Unlock()
		if nonEmpty {
			c.signal(c.changed)
		}
	case randr.ScreenChangeNotifyEvent:
		c.signal(c.resized)
	}
}
