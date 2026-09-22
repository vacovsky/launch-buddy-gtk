package main

// windows.go — the Output and Settings windows, in pure-Go GTK4. They mirror the
// Python reference: plain Gtk.Window (never Gtk.Application, so this app cannot
// fight the D-Bus service over org.launchbuddy.Gnome), a header bar as an
// ordinary child of the outer box, and close-requested is vetoed so a "closed"
// window is hidden, not destroyed (a Go uintptr has no refcount; a real destroy
// would leave a dangling handle). open() re-presents a hidden window.
//
// The Output window adds a 300 ms poll (the reference declares POLL_MS=300 but
// never wires it — the macOS original did). The poll re-renders the tail as new
// lines arrive, keeps a "Follow" view pinned to the newest line, and pauses
// auto-follow when the user scrolls up (detected via the scrolled window's
// v-adjustment, the Go-friendly equivalent of the reference's scroll-event).

import (
	"fmt"
	"log"
	"strings"
	"unsafe"

	"github.com/ebitengine/purego"
)

// GtkTextIter is ~80 bytes; 128 is a safe stack home for the value GTK writes
// through a GtkTextIter* out-pointer.
const iterBytes = 128

// ---- Output window ----------------------------------------------------------

type outputWindow struct {
	app          *app
	window       Widget
	buffer       Widget
	status       Widget // GtkLabel in the header
	followSwitch Widget // "Follow" GtkSwitch
	view         Widget
	scrolled     Widget

	follow bool
	shown  int // total lines rendered; -1 means "render the tail on open"
	pollID uint32
}

func newOutputWindow(a *app) *outputWindow {
	return &outputWindow{app: a, follow: true, shown: -1}
}

func (o *outputWindow) build() {
	if o.pollID != 0 {
		SourceRemove(o.pollID)
	}
	w := WindowNew()
	gtkWindowSetTitle(uintptr(w), "Launch Buddy — Output")
	gtkWindowSetDefaultSize(uintptr(w), 760, 480)

	header := Widget(gtkHeaderBarNew())
	gtkHeaderBarSetShowTitleButtons(uintptr(header), 0, 0)
	title := Widget(gtkLabelNew("Output"))
	o.status = Widget(gtkLabelNew("Stopped"))
	gtkLabelSetXAlign(uintptr(o.status), 0.0)
	gtkWidgetAddCssClass(uintptr(o.status), "dim-label")
	gtkHeaderBarSetTitleWidget(uintptr(header), uintptr(title))
	back := Widget(gtkButtonNewFromIconName("go-back-symbolic"))
	back.OnVoid("clicked", o.close)
	gtkHeaderBarPackStart(uintptr(header), uintptr(back))
	gtkHeaderBarPackEnd(uintptr(header), uintptr(o.status))

	box := Widget(gtkBoxNew(1, 0)) // VERTICAL, spacing 0
	gtkBoxAppend(uintptr(box), uintptr(header))

	row := Widget(gtkBoxNew(0, 8)) // HORIZONTAL, spacing 8
	gtkWidgetSetMarginStart(uintptr(row), 12)
	gtkWidgetSetMarginEnd(uintptr(row), 12)
	gtkWidgetSetMarginTop(uintptr(row), 8)
	gtkWidgetSetMarginBottom(uintptr(row), 4)
	clear := Widget(gtkButtonNewWithLabel("Clear"))
	clear.OnVoid("clicked", o.clear)
	gtkBoxAppend(uintptr(row), uintptr(clear))
	spacer := Widget(gtkBoxNew(0, 0))
	gtkWidgetSetHExpand(uintptr(spacer), true)
	gtkBoxAppend(uintptr(row), uintptr(spacer))
	gtkBoxAppend(uintptr(row), uintptr(gtkLabelNew("Follow")))
	sw := Widget(gtkSwitchNew())
	gtkSwitchSetActive(uintptr(sw), true)
	gtkWidgetSetValign(uintptr(sw), 3) // CENTER
	o.followSwitch = sw
	sw.OnStateSet(func(n int32) { o.setFollow(n != 0) })
	gtkBoxAppend(uintptr(row), uintptr(sw))
	gtkBoxAppend(uintptr(box), uintptr(row))

	view := Widget(gtkTextViewNew())
	gtkTextViewSetEditable(uintptr(view), false)
	gtkTextViewSetWrapMode(uintptr(view), 2) // WORD_CHAR
	gtkTextViewSetLeftMargin(uintptr(view), 10)
	gtkTextViewSetRightMargin(uintptr(view), 10)
	gtkTextViewSetTopMargin(uintptr(view), 4)
	gtkTextViewSetBottomMargin(uintptr(view), 4)
	gtkTextViewSetMonospace(uintptr(view), true)
	gtkTextViewSetCursorVisible(uintptr(view), false)
	gtkWidgetSetName(uintptr(view), "output-view")
	o.view = view
	o.buffer = Widget(gtkTextViewGetBuffer(uintptr(view)))

	sc := Widget(gtkScrolledWindowNew())
	gtkWidgetSetHExpand(uintptr(sc), true)
	gtkWidgetSetVExpand(uintptr(sc), true)
	gtkScrolledWindowSetPolicy(uintptr(sc), 1, 1) // AUTOMATIC, AUTOMATIC
	gtkScrolledWindowSetChild(uintptr(sc), uintptr(view))
	o.scrolled = sc
	gtkBoxAppend(uintptr(box), uintptr(sc))

	gtkWindowSetChild(uintptr(w), uintptr(box))
	o.window = w

	w.OnCloseRequested(o.close)
	o.pollID = TimeoutAdd(300, func() bool { o.poll(); return true })
}

func (o *outputWindow) open() {
	if o.window == 0 || !gtkWidgetGetVisible(uintptr(o.window)) {
		o.build()
	}
	gtkWindowPresent(uintptr(o.window))
	if o.shown < 0 {
		o.renderAll() // re-render the tail after a close/reset
	}
	o.refresh()
}

func (o *outputWindow) close() {
	if o.window == 0 {
		return
	}
	gtkWidgetHide(uintptr(o.window))
	o.shown = -1 // re-render the tail on next open
}

func (o *outputWindow) statusText() string {
	pm := o.app.pm
	if pm.Running() {
		return fmt.Sprintf("Running (pid %d)", pm.Pgid())
	}
	if e := pm.LaunchError(); e != "" {
		return "Error: " + e
	}
	return "Stopped"
}

// poll is the 300 ms driver: refresh the tail, then reconcile the Follow switch
// with the current scroll position. Runs on the GTK main thread.
func (o *outputWindow) poll() {
	o.refresh()
	o.followState()
}

func (o *outputWindow) refresh() {
	if o.buffer == 0 || !gtkWidgetGetVisible(uintptr(o.window)) {
		return
	}
	gtkLabelSetText(uintptr(o.status), o.statusText())
	pm := o.app.pm
	total := pm.TotalLines()
	if total == o.shown {
		return
	}
	if total < o.shown || gtkTextBufferGetCharCount(uintptr(o.buffer)) == 0 {
		o.renderAll()
	} else {
		o.appendNew()
	}
}

func (o *outputWindow) renderAll() {
	pm := o.app.pm
	gtkTextBufferSetText(uintptr(o.buffer), strings.Join(pm.LastLines(), "\n"), -1)
	o.shown = pm.TotalLines()
	if o.follow {
		o.scrollToEnd()
	}
}

func (o *outputWindow) appendNew() {
	pm := o.app.pm
	lines := pm.LastLines()
	total := pm.TotalLines()
	off := total - o.shown
	if off < 0 {
		off = 0
	}
	if off > len(lines) {
		off = len(lines)
	}
	new := lines[off:]
	var it [iterBytes]byte
	end := uintptr(unsafe.Pointer(&it[0]))
	gtkTextBufferGetEndIter(uintptr(o.buffer), end)
	if len(new) > 0 {
		gtkTextBufferInsert(uintptr(o.buffer), end, strings.Join(new, "\n"), -1)
		o.shown = total
	}
	if o.follow {
		o.scrollToEnd()
	}
}

func (o *outputWindow) scrollToEnd() {
	if o.buffer == 0 || o.view == 0 {
		return
	}
	var it [iterBytes]byte
	end := uintptr(unsafe.Pointer(&it[0]))
	gtkTextBufferGetEndIter(uintptr(o.buffer), end)
	mark := gtkTextBufferCreateMark(uintptr(o.buffer), 0, end, 0) // (buffer, name=NULL, iter, stick)
	gtkTextViewScrollToMark(uintptr(o.view), mark, 0.0, 1, 0.0, 1.0)
	gtkTextBufferDeleteMark(uintptr(o.buffer), mark)
}

// followState pauses auto-follow when the user has scrolled away from the very
// bottom (value + page < upper). Resumption is a manual action (the reference
// only auto-pauses; the user re-enables Follow to resume).
func (o *outputWindow) followState() {
	if o.scrolled == 0 || !o.follow {
		return
	}
	adj := gtkScrolledWindowGetVAdjustment(uintptr(o.scrolled))
	if adj == 0 {
		return
	}
	if gtkAdjustmentGetValue(adj)+gtkAdjustmentGetPageSize(adj) < gtkAdjustmentGetUpper(adj)-1.0 {
		o.setFollow(false)
	}
}

func (o *outputWindow) setFollow(on bool) {
	if o.follow == on {
		return
	}
	o.follow = on
	if o.followSwitch != 0 {
		gtkSwitchSetActive(uintptr(o.followSwitch), on)
	}
	if on {
		o.scrollToEnd()
	}
}

func (o *outputWindow) clear() {
	if o.buffer != 0 {
		gtkTextBufferSetText(uintptr(o.buffer), "", -1)
	}
	o.shown = o.app.pm.TotalLines()
}

// ---- Settings window --------------------------------------------------------

type settingsWindow struct {
	app    *app
	window Widget
	entry  Widget
}

func newSettingsWindow(a *app) *settingsWindow { return &settingsWindow{app: a} }

func (s *settingsWindow) build() {
	w := WindowNew()
	gtkWindowSetTitle(uintptr(w), "Launch Buddy — Settings")
	gtkWindowSetDefaultSize(uintptr(w), 520, 460)

	header := Widget(gtkHeaderBarNew())
	gtkHeaderBarSetShowTitleButtons(uintptr(header), 0, 0)
	title := Widget(gtkLabelNew("Settings"))
	gtkHeaderBarSetTitleWidget(uintptr(header), uintptr(title))
	back := Widget(gtkButtonNewFromIconName("go-back-symbolic"))
	back.OnVoid("clicked", s.close)
	gtkHeaderBarPackStart(uintptr(header), uintptr(back))

	box := Widget(gtkBoxNew(1, 0)) // VERTICAL
	gtkBoxAppend(uintptr(box), uintptr(header))

	body := Widget(gtkBoxNew(1, 16)) // VERTICAL, spacing 16
	gtkWidgetSetMarginStart(uintptr(body), 24)
	gtkWidgetSetMarginEnd(uintptr(body), 24)
	gtkWidgetSetMarginTop(uintptr(body), 16)
	gtkWidgetSetMarginBottom(uintptr(body), 16)

	heading := func(t string) {
		l := Widget(gtkLabelNew(t))
		gtkLabelSetXAlign(uintptr(l), 0.0)
		gtkWidgetAddCssClass(uintptr(l), "heading")
		gtkBoxAppend(uintptr(body), uintptr(l))
	}

	heading("Script")
	pathRow := Widget(gtkBoxNew(0, 8)) // HORIZONTAL, spacing 8
	e := Widget(gtkEntryNew())
	gtkWidgetSetHExpand(uintptr(e), true)
	gtkEditableSetText(uintptr(e), s.app.s.ScriptPath)
	e.OnVoid("changed", func() { s.pathChanged() })
	s.entry = e
	gtkBoxAppend(uintptr(pathRow), uintptr(e))
	browse := Widget(gtkButtonNewWithLabel("Browse…"))
	browse.OnVoid("clicked", func() { s.browse(w) })
	gtkBoxAppend(uintptr(pathRow), uintptr(browse))
	gtkBoxAppend(uintptr(body), uintptr(pathRow))

	heading("Launch at login")
	launch := s.switchRow("Start Launch Buddy when you log in.",
		s.app.s.LaunchAtLogin, func(n int32) { s.launchChanged(n) })
	gtkBoxAppend(uintptr(body), uintptr(launch))

	heading("Restore on launch")
	restore := s.switchRow("If a script was running when you last quit, start it again.",
		s.app.s.RestoreOnLaunch, func(n int32) { s.restoreChanged(n) })
	gtkBoxAppend(uintptr(body), uintptr(restore))

	sc := Widget(gtkScrolledWindowNew())
	gtkWidgetSetHExpand(uintptr(sc), true)
	gtkWidgetSetVExpand(uintptr(sc), true)
	gtkScrolledWindowSetPolicy(uintptr(sc), 0, 1) // NEVER, AUTOMATIC
	gtkScrolledWindowSetChild(uintptr(sc), uintptr(body))
	gtkBoxAppend(uintptr(box), uintptr(sc))

	gtkWindowSetChild(uintptr(w), uintptr(box))
	s.window = w
	w.OnCloseRequested(s.close)
}

// switchRow builds a centered GtkSwitch plus a dimmed, left-justified
// description beneath it, wired to fn (which receives the new 0/1 state).
func (s *settingsWindow) switchRow(desc string, active bool, fn func(int32)) Widget {
	sw := Widget(gtkSwitchNew())
	gtkSwitchSetActive(uintptr(sw), active)
	gtkWidgetSetHalign(uintptr(sw), 1) // START (GtkAlign: FILL=0, START=1): pin switch to the left edge, aligned with the heading
	gtkWidgetSetValign(uintptr(sw), 3) // CENTER
	sw.OnStateSet(fn)
	dl := Widget(gtkLabelNew(desc))
	gtkLabelSetXAlign(uintptr(dl), 0.0)
	gtkLabelSetWrap(uintptr(dl), true)
	gtkLabelSetJustify(uintptr(dl), 0) // LEFT
	gtkWidgetAddCssClass(uintptr(dl), "dim-label")
	row := Widget(gtkBoxNew(1, 4)) // VERTICAL, spacing 4
	gtkBoxAppend(uintptr(row), uintptr(sw))
	gtkBoxAppend(uintptr(row), uintptr(dl))
	return row
}

func (s *settingsWindow) open() {
	if s.window == 0 || !gtkWidgetGetVisible(uintptr(s.window)) {
		s.build()
	}
	gtkWindowPresent(uintptr(s.window))
}

func (s *settingsWindow) close() {
	if s.window == 0 {
		return
	}
	gtkWidgetHide(uintptr(s.window))
}

func (s *settingsWindow) pathChanged() {
	if s.entry == 0 {
		return
	}
	s.app.s.ScriptPath = strings.TrimSpace(gtkEditableGetText(uintptr(s.entry)))
	s.app.save()
}

func (s *settingsWindow) launchChanged(n int32) {
	s.app.s.LaunchAtLogin = n != 0
	if err := SetAutostart(s.app.s.LaunchAtLogin, s.app.exePath, s.app.exeDir); err != nil {
		log.Printf("autostart: %v", err)
	}
	s.app.save()
}

func (s *settingsWindow) restoreChanged(n int32) {
	s.app.s.RestoreOnLaunch = n != 0
	s.app.save()
}

// browse opens a modal file picker. gtk_file_dialog_open is a GTask: its
// ready callback (source, task, user_data) is invoked on the GTK main thread,
// so it may touch the entry directly.
func (s *settingsWindow) browse(parent Widget) {
	dlg := Widget(gtkFileDialogNew())
	gtkFileDialogSetTitle(uintptr(dlg), "Choose a script")
	gtkFileDialogSetModal(uintptr(dlg), uintptr(parent))
	cb := purego.NewCallback(func(_ uintptr, task uintptr, _ uintptr) {
		arr := gtkFileOpenFinish(uintptr(dlg), task, 0)
		if arr == 0 {
			return
		}
		first := firstPath(arr)
		gStrFreev(arr)
		if first != "" && s.entry != 0 {
			gtkEditableSetText(uintptr(s.entry), first)
		}
	})
	retainCallback(cb)
	gtkFileDialogOpen(uintptr(dlg), 0, uintptr(cb), 0)
}

// firstPath reads the first char* out of a char** returned by the file dialog.
func firstPath(arr uintptr) string {
	if arr == 0 {
		return ""
	}
	p := *(*uintptr)(unsafe.Pointer(arr))
	return cstr(p)
}
