package ui

import (
	"image"
)

// Widget is a control a screen places and hands events to. Widgets are
// plain structs: a screen composes them and owns the values they edit.
type Widget interface {
	Bounds() image.Rectangle
	Paint(p *Painter, focused bool)
	// Key handles a key press when the widget has focus.
	Key(ev KeyEvent) bool
	// Mouse handles a pointer event in window coordinates.
	Mouse(ev MouseEvent) bool
	Focusable() bool
}

// Label is a line of text.
type Label struct {
	X, Y int
	Text string
	Dim  bool
}

func (l *Label) Bounds() image.Rectangle {
	return image.Rect(l.X, l.Y, l.X+l.TextWidth(), l.Y+RowHeight)
}
func (l *Label) TextWidth() int { return len(l.Text) * FontWidth }
func (l *Label) Paint(p *Painter, _ bool) {
	c := Fg
	if l.Dim {
		c = Dim
	}
	p.Text(l.X, l.Y+TextBaseline, l.Text, c)
}
func (l *Label) Key(KeyEvent) bool     { return false }
func (l *Label) Mouse(MouseEvent) bool { return false }
func (l *Label) Focusable() bool       { return false }

// Button is a rectangle that does one thing.
type Button struct {
	X, Y, W  int
	Label    string
	Primary  bool
	Danger   bool
	Disabled bool
	OnClick  func()
	pressed  bool
}

func (b *Button) Bounds() image.Rectangle {
	return image.Rect(b.X, b.Y, b.X+b.W, b.Y+RowHeight)
}

func (b *Button) Paint(p *Painter, focused bool) {
	r := b.Bounds()
	bg, fg := PanelHi, Fg
	switch {
	case b.Danger:
		bg, fg = Danger, AccentFg
	case b.Primary:
		bg, fg = Accent, AccentFg
	}
	if b.pressed {
		bg = Panel
	}
	if b.Disabled {
		bg, fg = Panel, Faint
	}
	p.Fill(r, bg)
	if focused {
		p.Rect(r.Inset(-2), Accent)
	} else {
		p.Rect(r, Border)
	}
	text := Truncate(b.Label, b.W-2*FontWidth)
	p.Text(r.Min.X+(b.W-p.TextWidth(text))/2, r.Min.Y+TextBaseline, text, fg)
}

func (b *Button) Key(ev KeyEvent) bool {
	if b.Disabled {
		return false
	}
	if ev.Enter || ev.Space {
		b.fire()
		return true
	}
	return false
}

func (b *Button) Mouse(ev MouseEvent) bool {
	if b.Disabled {
		return false
	}
	switch ev.Kind {
	case MousePress:
		if b.Bounds().Overlaps(image.Rect(ev.X, ev.Y, ev.X+1, ev.Y+1)) {
			b.pressed = true
			return true
		}
	case MouseRelease:
		changed := b.pressed
		b.pressed = false
		if changed && b.Bounds().Overlaps(image.Rect(ev.X, ev.Y, ev.X+1, ev.Y+1)) {
			b.fire()
		}
		return changed
	}
	return false
}

func (b *Button) fire() {
	if b.OnClick != nil {
		b.OnClick()
	}
}

func (b *Button) Focusable() bool { return !b.Disabled }

// Field is one line of text to edit. It edits the string behind Value.
type Field struct {
	X, Y, W  int
	Value    *string
	Hint     string // shown while empty
	OnSubmit func()
	Cursor   int // position in runes
}

func (f *Field) Bounds() image.Rectangle {
	return image.Rect(f.X, f.Y, f.X+f.W, f.Y+RowHeight)
}

func (f *Field) Paint(p *Painter, focused bool) {
	r := f.Bounds()
	p.Fill(r, Input)
	if focused {
		p.Rect(r, Accent)
	} else {
		p.Rect(r, Border)
	}
	value := f.text()
	if value == "" && !focused {
		p.Text(r.Min.X+FontWidth, r.Min.Y+TextBaseline, Truncate(f.Hint, f.W-2*FontWidth), Faint)
		return
	}
	// The cursor must be visible: scroll the text so it is.
	runes := []rune(value)
	if f.Cursor > len(runes) {
		f.Cursor = len(runes)
	}
	shown, off := fitCursor(runes, f.Cursor, f.W-2*FontWidth)
	p.Text(r.Min.X+FontWidth, r.Min.Y+TextBaseline, shown, Fg)
	if focused {
		cx := r.Min.X + FontWidth + (f.Cursor-off)*FontWidth
		p.Fill(image.Rect(cx, r.Min.Y+4, cx+2, r.Min.Y+RowHeight-4), Accent)
	}
}

// fitCursor keeps the cursor in view, returning the visible slice and
// where it starts.
func fitCursor(runes []rune, cursor, width int) (string, int) {
	cols := width / FontWidth
	if len(runes) <= cols {
		return string(runes), 0
	}
	off := cursor - cols + 1
	if off < 0 {
		off = 0
	}
	end := off + cols
	if end > len(runes) {
		end = len(runes)
	}
	return string(runes[off:end]), off
}

func (f *Field) text() string {
	if f.Value == nil {
		return ""
	}
	return *f.Value
}

func (f *Field) setText(s string) {
	if f.Value != nil {
		*f.Value = s
	}
}

// Key edits the text: characters insert, the named keys edit and move.
func (f *Field) Key(ev KeyEvent) bool {
	runes := []rune(f.text())
	switch {
	case ev.Enter:
		if f.OnSubmit != nil {
			f.OnSubmit()
		}
	case ev.Bksp:
		if f.Cursor > 0 {
			runes = append(runes[:f.Cursor-1], runes[f.Cursor:]...)
			f.Cursor--
			f.setText(string(runes))
		}
	case ev.Delete:
		if f.Cursor < len(runes) {
			runes = append(runes[:f.Cursor], runes[f.Cursor+1:]...)
			f.setText(string(runes))
		}
	case ev.Left:
		if f.Cursor > 0 {
			f.Cursor--
		}
	case ev.Right:
		if f.Cursor < len(runes) {
			f.Cursor++
		}
	case ev.Home:
		f.Cursor = 0
	case ev.End:
		f.Cursor = len(runes)
	case ev.Ctrl && ev.Char == 'u':
		f.setText("")
		f.Cursor = 0
	case ev.Char != 0:
		runes = append(runes[:f.Cursor], append([]rune{ev.Char}, runes[f.Cursor:]...)...)
		f.Cursor++
		f.setText(string(runes))
	default:
		return false
	}
	return true
}

func (f *Field) Mouse(ev MouseEvent) bool {
	if ev.Kind != MousePress || !f.Bounds().Overlaps(image.Rect(ev.X, ev.Y, ev.X+1, ev.Y+1)) {
		return false
	}
	// Clicking puts the cursor where the click landed.
	f.Cursor = (ev.X - f.X - FontWidth) / FontWidth
	if f.Cursor < 0 {
		f.Cursor = 0
	}
	if n := len([]rune(f.text())); f.Cursor > n {
		f.Cursor = n
	}
	return true
}

func (f *Field) Focusable() bool { return true }

// Check is a box that is on or off.
type Check struct {
	X, Y  int
	Label string
	Value *bool
}

func (c *Check) Bounds() image.Rectangle {
	return image.Rect(c.X, c.Y, c.X+18+len(c.Label)*FontWidth, c.Y+RowHeight)
}

func (c *Check) Paint(p *Painter, focused bool) {
	box := image.Rect(c.X, c.Y+4, c.X+14, c.Y+18)
	p.Fill(box, Input)
	p.Rect(box, Border)
	if c.on() {
		p.Fill(box.Inset(3), Accent)
	}
	if focused {
		p.Rect(box.Inset(-2), Accent)
	}
	p.Text(c.X+20, c.Y+TextBaseline, c.Label, Fg)
}

func (c *Check) on() bool { return c.Value != nil && *c.Value }

func (c *Check) toggle() {
	if c.Value != nil {
		*c.Value = !*c.Value
	}
}

func (c *Check) Key(ev KeyEvent) bool {
	if ev.Space || ev.Enter {
		c.toggle()
		return true
	}
	return false
}

func (c *Check) Mouse(ev MouseEvent) bool {
	if ev.Kind == MousePress && c.Bounds().Overlaps(image.Rect(ev.X, ev.Y, ev.X+1, ev.Y+1)) {
		c.toggle()
		return true
	}
	return false
}

func (c *Check) Focusable() bool { return true }

// Radio is a row of options with one chosen. Left/Right (or Tab-like
// clicks) move between them.
type Radio struct {
	X, Y    int
	Options []string
	Value   *string
}

func (r *Radio) Bounds() image.Rectangle {
	w := 0
	for _, o := range r.Options {
		w += 24 + len(o)*FontWidth
	}
	return image.Rect(r.X, r.Y, r.X+w, r.Y+RowHeight)
}

func (r *Radio) Paint(p *Painter, focused bool) {
	x := r.X
	for _, o := range r.Options {
		w := 24 + len(o)*FontWidth
		box := image.Rect(x, r.Y+4, x+14, r.Y+18)
		p.Fill(box, Input)
		p.Rect(box, Border)
		if r.Value != nil && *r.Value == o {
			p.Fill(box.Inset(3), Accent)
		}
		if focused && r.Value != nil && *r.Value == o {
			p.Rect(box.Inset(-2), Accent)
		}
		p.Text(x+20, r.Y+TextBaseline, o, Fg)
		x += w
	}
}

func (r *Radio) choose(i int) {
	if r.Value != nil && i >= 0 && i < len(r.Options) {
		*r.Value = r.Options[i]
	}
}

func (r *Radio) index() int {
	for i, o := range r.Options {
		if r.Value != nil && *r.Value == o {
			return i
		}
	}
	return -1
}

func (r *Radio) Key(ev KeyEvent) bool {
	i := r.index()
	switch {
	case ev.Left:
		r.choose(max(i-1, 0))
	case ev.Right:
		r.choose(min(i+1, len(r.Options)-1))
	default:
		return false
	}
	return true
}

func (r *Radio) Mouse(ev MouseEvent) bool {
	if ev.Kind != MousePress {
		return false
	}
	x := r.X
	for i, o := range r.Options {
		w := 24 + len(o)*FontWidth
		if image.Rect(x, r.Y, x+w, r.Y+RowHeight).Overlaps(image.Rect(ev.X, ev.Y, ev.X+1, ev.Y+1)) {
			r.choose(i)
			return true
		}
		x += w
	}
	return false
}

func (r *Radio) Focusable() bool { return true }

// Select is a choice from a short list, closed until asked. It takes its
// value from the same string a Radio would.
type Select struct {
	X, Y, W   int
	Options   []string
	Value     *string
	open      bool
	highlight int
}

func (s *Select) Bounds() image.Rectangle {
	return image.Rect(s.X, s.Y, s.X+s.W, s.Y+RowHeight)
}

func (s *Select) Paint(p *Painter, focused bool) {
	r := s.Bounds()
	p.Fill(r, Input)
	if focused || s.open {
		p.Rect(r, Accent)
	} else {
		p.Rect(r, Border)
	}
	text := ""
	if s.Value != nil {
		text = *s.Value
	}
	p.Text(r.Min.X+FontWidth, r.Min.Y+TextBaseline, Truncate(text, s.W-20), Fg)
	p.Text(r.Max.X-14, r.Min.Y+TextBaseline, "v", Dim)
	if s.open {
		s.paintList(p)
	}
}

// paintList draws the open dropdown over whatever is beneath it.
func (s *Select) paintList(p *Painter) {
	r := image.Rect(s.X, s.Y+RowHeight, s.X+s.W, s.Y+RowHeight+min(len(s.Options), 8)*RowHeight)
	p.Fill(r, Panel)
	p.Rect(r, Border)
	for i, o := range s.Options {
		if i >= 8 {
			break
		}
		row := image.Rect(r.Min.X+1, r.Min.Y+1+i*RowHeight, r.Max.X-1, r.Min.Y+1+(i+1)*RowHeight)
		if i == s.highlight {
			p.Fill(row, Accent)
		}
		c := Fg
		if i == s.highlight {
			c = AccentFg
		}
		p.Text(row.Min.X+FontWidth, row.Min.Y+TextBaseline, Truncate(o, s.W-20), c)
	}
}

func (s *Select) popupRow(y int) int {
	if y < s.Y+RowHeight {
		return -1
	}
	return (y - s.Y - RowHeight) / RowHeight
}

func (s *Select) Key(ev KeyEvent) bool {
	switch {
	case ev.Enter || ev.Space:
		s.open = !s.open
		s.highlight = s.index()
		return true
	case ev.Escape:
		if s.open {
			s.open = false
			return true
		}
	case s.open && (ev.Up || ev.Down):
		if ev.Up {
			s.highlight = max(s.highlight-1, 0)
		} else {
			s.highlight = min(s.highlight+1, len(s.Options)-1)
		}
		return true
	}
	if !s.open && (ev.Up || ev.Down) {
		// Closed: arrows move the choice, like a radio group.
		i := s.index()
		if ev.Up {
			s.choose(max(i-1, 0))
		} else {
			s.choose(min(i+1, len(s.Options)-1))
		}
		return true
	}
	return false
}

func (s *Select) Mouse(ev MouseEvent) bool {
	if s.open {
		switch ev.Kind {
		case MousePress:
			if row := s.popupRow(ev.Y); row >= 0 && row < len(s.Options) {
				s.choose(row)
				s.open = false
				return true
			}
			s.open = false
			return true
		case MouseMove:
			if row := s.popupRow(ev.Y); row >= 0 && row < len(s.Options) && row != s.highlight {
				s.highlight = row
				return true
			}
		}
		return false
	}
	if ev.Kind == MousePress && s.Bounds().Overlaps(image.Rect(ev.X, ev.Y, ev.X+1, ev.Y+1)) {
		s.open = true
		s.highlight = s.index()
		return true
	}
	return false
}

func (s *Select) choose(i int) {
	if s.Value != nil && i >= 0 && i < len(s.Options) {
		*s.Value = s.Options[i]
	}
}

func (s *Select) index() int {
	for i, o := range s.Options {
		if s.Value != nil && *s.Value == o {
			return i
		}
	}
	return 0
}

func (s *Select) Focusable() bool { return true }

// Open reports whether the dropdown is showing its list.
func (s *Select) Open() bool { return s.open }

// List is a scrollable list of rows: the speed dial.
type List struct {
	X, Y, W, H int
	Items      []string
	Sub        []string // a dim second line per item, may be empty
	Selected   *int
	OnActivate func(i int) // Enter or double-click
	scroll     int
}

func (l *List) Bounds() image.Rectangle {
	return image.Rect(l.X, l.Y, l.X+l.W, l.Y+l.H)
}

func (l *List) rows() int { return max(l.H/RowHeight, 1) }

func (l *List) Paint(p *Painter, focused bool) {
	r := l.Bounds()
	p.Fill(r, Input)
	p.Rect(r, Border)
	// Keep the selection in view.
	if l.Selected != nil {
		if *l.Selected < l.scroll {
			l.scroll = *l.Selected
		}
		if *l.Selected >= l.scroll+l.rows() {
			l.scroll = *l.Selected - l.rows() + 1
		}
	}
	for i := 0; i < l.rows(); i++ {
		idx := l.scroll + i
		if idx >= len(l.Items) {
			break
		}
		row := image.Rect(r.Min.X+1, r.Min.Y+1+i*RowHeight, r.Max.X-1, r.Min.Y+1+(i+1)*RowHeight)
		selected := l.Selected != nil && *l.Selected == idx
		if selected {
			p.Fill(row, Accent)
		}
		c := Fg
		if selected {
			c = AccentFg
		}
		p.Text(row.Min.X+FontWidth, row.Min.Y+14, Truncate(l.Items[idx], l.W-4*FontWidth), c)
		if l.Sub != nil && idx < len(l.Sub) && l.Sub[idx] != "" {
			sc := Dim
			if selected {
				sc = AccentFg
			}
			p.Text(row.Min.X+FontWidth, row.Min.Y+28, Truncate(l.Sub[idx], l.W-4*FontWidth), sc)
		}
		if selected && focused {
			p.Rect(row.Inset(-1), AccentFg)
		}
	}
}

func (l *List) rowAt(y int) int {
	if y < l.Y+1 || y >= l.Y+l.H {
		return -1
	}
	i := l.scroll + (y-l.Y-1)/RowHeight
	if i >= len(l.Items) {
		return -1
	}
	return i
}

func (l *List) Key(ev KeyEvent) bool {
	if l.Selected == nil || len(l.Items) == 0 {
		return false
	}
	i := *l.Selected
	switch {
	case ev.Up:
		i--
	case ev.Down:
		i++
	case ev.PageUp:
		i -= l.rows()
	case ev.PageDn:
		i += l.rows()
	case ev.Home:
		i = 0
	case ev.End:
		i = len(l.Items) - 1
	case ev.Enter:
		if l.OnActivate != nil {
			l.OnActivate(*l.Selected)
		}
		return true
	default:
		return false
	}
	*l.Selected = min(max(i, 0), len(l.Items)-1)
	return true
}

func (l *List) Mouse(ev MouseEvent) bool {
	switch ev.Kind {
	case MousePress:
		if i := l.rowAt(ev.Y); i >= 0 {
			if l.Selected != nil {
				*l.Selected = i
			}
			return true
		}
	case MouseWheelUp:
		l.scroll = max(l.scroll-3, 0)
		return true
	case MouseWheelDown:
		l.scroll = min(l.scroll+3, max(len(l.Items)-l.rows(), 0))
		return true
	}
	return false
}

func (l *List) Focusable() bool { return true }

// Form is a page of widgets: it paints them all, and Tab moves focus
// between the focusable ones. A screen owns the form and paints whatever
// else it needs around it.
type Form struct {
	Widgets []Widget
	Focus   int
}

// Add appends widgets and returns the form, for building in one breath.
func (f *Form) Add(widgets ...Widget) *Form {
	f.Widgets = append(f.Widgets, widgets...)
	return f
}

func (f *Form) Paint(p *Painter) {
	for i, w := range f.Widgets {
		w.Paint(p, i == f.Focus)
	}
}

// Key gives the key to the focused widget, moving focus on Tab.
func (f *Form) Key(ev KeyEvent) bool {
	if ev.Tab {
		step := 1
		if ev.Shift {
			step = -1
		}
		f.move(step)
		return true
	}
	if w := f.focused(); w != nil {
		return w.Key(ev)
	}
	return false
}

// Mouse gives the event to the widget it landed on, and focuses it.
func (f *Form) Mouse(ev MouseEvent) bool {
	// An open dropdown owns the pointer until it closes.
	for i, w := range f.Widgets {
		if s, ok := w.(*Select); ok && s.Open() {
			if i != f.Focus {
				f.Focus = i
			}
			return s.Mouse(ev)
		}
	}
	if ev.Kind != MousePress {
		// Motion and wheel still matter to whoever is under them.
		for _, w := range f.Widgets {
			if _, isList := w.(*List); !isList {
				continue
			}
			if w.Mouse(ev) {
				return true
			}
		}
		return false
	}
	for i, w := range f.Widgets {
		if !w.Focusable() {
			continue
		}
		if w.Bounds().Overlaps(image.Rect(ev.X, ev.Y, ev.X+1, ev.Y+1)) {
			f.Focus = i
			return w.Mouse(ev)
		}
	}
	return false
}

// focused is the widget with focus, skipping labels and their kin.
func (f *Form) focused() Widget {
	if f.Focus >= 0 && f.Focus < len(f.Widgets) {
		return f.Widgets[f.Focus]
	}
	return nil
}

// move walks focus to the next focusable widget.
func (f *Form) move(step int) {
	if len(f.Widgets) == 0 {
		return
	}
	for n := 0; n < len(f.Widgets); n++ {
		f.Focus = (f.Focus + step + len(f.Widgets)) % len(f.Widgets)
		if f.Widgets[f.Focus].Focusable() {
			return
		}
	}
}

// FocusFirst puts the focus on the first control that wants it.
func (f *Form) FocusFirst() {
	f.Focus = -1
	f.move(1)
}
