package main

// gtk_glue.go — pure-Go GTK4 bridge over github.com/ebitengine/purego with
// CGO_ENABLED=0. libgtk-4 / libgobject-2.0 / libglib-2.0 / libgio-2.0 are
// dlopen'd by soname and every entry point is resolved with
// purego.RegisterLibFunc. No cgo, no -dev packages.
//
// A GObject signal reaches Go through purego.NewCallback, so control clicks,
// edits, state flips and async file-dialog completions are ordinary Go
// closures. Every callback is retained for the process life: GTK stores the C
// function pointer and may invoke it at any later time, so the Go closure must
// never be collected.

import (
	"fmt"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// A Widget is a GTK widget — a GObject pointer. The zero value is the null
// widget. A MainLoop is a GLib main loop.
type Widget uintptr
type MainLoop uintptr

var (
	loadOnce sync.Once
	loadErr  error

	// glib
	gMainLoopNew        func(uintptr, bool) uintptr
	gMainLoopRun        func(uintptr)
	gMainLoopQuit       func(uintptr)
	gIdleAdd            func(uintptr, uintptr) uint32
	gTimeoutAdd         func(uintptr, uintptr) uint32
	gSourceRemove       func(uint32)
	gStrFreev           func(uintptr)
	gSetPrgname         func(string)
	gSetApplicationName func(string)

	// gobject
	gSignalConnectData func(uintptr, string, uintptr, uintptr, uintptr, int32) uint64

	// gtk
	gtkInitCheck                    func() bool
	gtkWindowNew                    func() uintptr
	gtkWindowSetTitle               func(uintptr, string)
	gtkWindowSetDefaultSize         func(uintptr, int32, int32)
	gtkWindowSetChild               func(uintptr, uintptr)
	gtkWindowPresent                func(uintptr)
	gtkHeaderBarNew                 func() uintptr
	gtkHeaderBarPackStart           func(uintptr, uintptr)
	gtkHeaderBarPackEnd             func(uintptr, uintptr)
	gtkHeaderBarSetShowTitleButtons func(uintptr, int32, int32)
	gtkHeaderBarSetTitleWidget      func(uintptr, uintptr)
	gtkBoxNew                       func(int32, int32) uintptr
	gtkBoxAppend                    func(uintptr, uintptr)
	gtkBoxSetSpacing                func(uintptr, int32)
	gtkLabelNew                     func(string) uintptr
	gtkLabelSetText                 func(uintptr, string)
	gtkLabelSetXAlign               func(uintptr, float64)
	gtkLabelSetWrap                 func(uintptr, bool)
	gtkLabelSetJustify              func(uintptr, int32)
	gtkButtonNewWithLabel           func(string) uintptr
	gtkButtonNewFromIconName        func(string) uintptr
	gtkEntryNew                     func() uintptr
	gtkEditableSetText              func(uintptr, string)
	gtkEditableGetText              func(uintptr) string
	gtkSwitchNew                    func() uintptr
	gtkSwitchSetActive              func(uintptr, bool)
	gtkSwitchGetActive              func(uintptr) bool
	gtkTextViewNew                  func() uintptr
	gtkTextViewSetMonospace         func(uintptr, bool)
	gtkTextViewSetEditable          func(uintptr, bool)
	gtkTextViewSetCursorVisible     func(uintptr, bool)
	gtkTextViewSetLeftMargin        func(uintptr, int32)
	gtkTextViewSetRightMargin       func(uintptr, int32)
	gtkTextViewSetTopMargin         func(uintptr, int32)
	gtkTextViewSetBottomMargin      func(uintptr, int32)
	gtkTextViewSetWrapMode          func(uintptr, int32)
	gtkTextViewScrollToMark         func(uintptr, uintptr, float64, int32, float64, float64)
	gtkTextViewGetBuffer            func(uintptr) uintptr
	gtkTextBufferSetText            func(uintptr, string, int32)
	gtkTextBufferGetStartIter       func(uintptr, uintptr)
	gtkTextBufferGetEndIter         func(uintptr, uintptr)
	gtkTextBufferInsert             func(uintptr, uintptr, string, int32)
	gtkTextBufferCreateMark         func(uintptr, uintptr, uintptr, int32) uintptr
	gtkTextBufferDeleteMark         func(uintptr, uintptr)
	gtkTextBufferGetCharCount       func(uintptr) int32
	gtkScrolledWindowNew            func() uintptr
	gtkScrolledWindowSetPolicy      func(uintptr, int32, int32)
	gtkScrolledWindowSetChild       func(uintptr, uintptr)
	gtkScrolledWindowGetVAdjustment func(uintptr) uintptr
	gtkAdjustmentGetValue           func(uintptr) float64
	gtkAdjustmentGetUpper           func(uintptr) float64
	gtkAdjustmentGetPageSize        func(uintptr) float64
	gtkWidgetSetHExpand             func(uintptr, bool)
	gtkWidgetSetVExpand             func(uintptr, bool)
	gtkWidgetSetHalign              func(uintptr, int32)
	gtkWidgetSetValign              func(uintptr, int32)
	gtkWidgetSetName                func(uintptr, string)
	gtkWidgetSetMarginStart         func(uintptr, int32)
	gtkWidgetSetMarginEnd           func(uintptr, int32)
	gtkWidgetSetMarginTop           func(uintptr, int32)
	gtkWidgetSetMarginBottom        func(uintptr, int32)
	gtkWidgetAddCssClass            func(uintptr, string)
	gtkWidgetSetVisible             func(uintptr, bool)
	gtkWidgetGetVisible             func(uintptr) bool
	gtkWidgetHide                   func(uintptr)
	gtkFileDialogNew                func() uintptr
	gtkFileDialogSetTitle           func(uintptr, string)
	gtkFileDialogSetModal           func(uintptr, uintptr)
	gtkFileDialogOpen               func(uintptr, uintptr, uintptr, uintptr, uintptr)
	gtkFileOpenFinish               func(uintptr, uintptr, uintptr) uintptr // symbol: gtk_file_dialog_open_finish
)

// dlopenAny tries each candidate soname (with and without the extension) and
// returns the first handle that loads.
func dlopenAny(names ...string) (uintptr, error) {
	var last error
	for _, n := range names {
		if h, err := purego.Dlopen(n, purego.RTLD_NOW|purego.RTLD_GLOBAL); err == nil {
			return h, nil
		} else {
			last = err
		}
	}
	return 0, fmt.Errorf("dlopen %v: %w", names, last)
}

func load() error {
	loadOnce.Do(func() {
		glib, err := dlopenAny("libglib-2.0.so.0", "libglib-2.0.so")
		if err != nil {
			loadErr = err
			return
		}
		gio, err := dlopenAny("libgio-2.0.so.0", "libgio-2.0.so")
		if err != nil {
			loadErr = err
			return
		}
		_ = gio // libgio is loaded for its GTask/GCancellable symbols GTK needs
		gobj, err := dlopenAny("libgobject-2.0.so.0", "libgobject-2.0.so")
		if err != nil {
			loadErr = err
			return
		}
		gtk, err := dlopenAny("libgtk-4.so.1", "libgtk-4.so")
		if err != nil {
			loadErr = err
			return
		}
		reg := func(p any, h uintptr, name string) { purego.RegisterLibFunc(p, h, name) }

		// glib
		reg(&gMainLoopNew, glib, "g_main_loop_new")
		reg(&gMainLoopRun, glib, "g_main_loop_run")
		reg(&gMainLoopQuit, glib, "g_main_loop_quit")
		reg(&gIdleAdd, glib, "g_idle_add")
		reg(&gTimeoutAdd, glib, "g_timeout_add")
		reg(&gSourceRemove, glib, "g_source_remove")
		reg(&gStrFreev, glib, "g_strfreev")
		reg(&gSetPrgname, glib, "g_set_prgname")
		reg(&gSetApplicationName, glib, "g_set_application_name")

		// gobject
		reg(&gSignalConnectData, gobj, "g_signal_connect_data")

		// gtk
		reg(&gtkInitCheck, gtk, "gtk_init_check")
		reg(&gtkWindowNew, gtk, "gtk_window_new")
		reg(&gtkWindowSetTitle, gtk, "gtk_window_set_title")
		reg(&gtkWindowSetDefaultSize, gtk, "gtk_window_set_default_size")
		reg(&gtkWindowSetChild, gtk, "gtk_window_set_child")
		reg(&gtkWindowPresent, gtk, "gtk_window_present")
		reg(&gtkHeaderBarNew, gtk, "gtk_header_bar_new")
		reg(&gtkHeaderBarPackStart, gtk, "gtk_header_bar_pack_start")
		reg(&gtkHeaderBarPackEnd, gtk, "gtk_header_bar_pack_end")
		reg(&gtkHeaderBarSetShowTitleButtons, gtk, "gtk_header_bar_set_show_title_buttons")
		reg(&gtkHeaderBarSetTitleWidget, gtk, "gtk_header_bar_set_title_widget")
		reg(&gtkBoxNew, gtk, "gtk_box_new")
		reg(&gtkBoxAppend, gtk, "gtk_box_append")
		reg(&gtkBoxSetSpacing, gtk, "gtk_box_set_spacing")
		reg(&gtkLabelNew, gtk, "gtk_label_new")
		reg(&gtkLabelSetText, gtk, "gtk_label_set_text")
		reg(&gtkLabelSetXAlign, gtk, "gtk_label_set_xalign")
		reg(&gtkLabelSetWrap, gtk, "gtk_label_set_wrap")
		reg(&gtkLabelSetJustify, gtk, "gtk_label_set_justify")
		reg(&gtkButtonNewWithLabel, gtk, "gtk_button_new_with_label")
		reg(&gtkButtonNewFromIconName, gtk, "gtk_button_new_from_icon_name")
		reg(&gtkEntryNew, gtk, "gtk_entry_new")
		reg(&gtkEditableSetText, gtk, "gtk_editable_set_text")
		reg(&gtkEditableGetText, gtk, "gtk_editable_get_text")
		reg(&gtkSwitchNew, gtk, "gtk_switch_new")
		reg(&gtkSwitchSetActive, gtk, "gtk_switch_set_active")
		reg(&gtkSwitchGetActive, gtk, "gtk_switch_get_active")
		reg(&gtkTextViewNew, gtk, "gtk_text_view_new")
		reg(&gtkTextViewSetMonospace, gtk, "gtk_text_view_set_monospace")
		reg(&gtkTextViewSetEditable, gtk, "gtk_text_view_set_editable")
		reg(&gtkTextViewSetCursorVisible, gtk, "gtk_text_view_set_cursor_visible")
		reg(&gtkTextViewSetLeftMargin, gtk, "gtk_text_view_set_left_margin")
		reg(&gtkTextViewSetRightMargin, gtk, "gtk_text_view_set_right_margin")
		reg(&gtkTextViewSetTopMargin, gtk, "gtk_text_view_set_top_margin")
		reg(&gtkTextViewSetBottomMargin, gtk, "gtk_text_view_set_bottom_margin")
		reg(&gtkTextViewSetWrapMode, gtk, "gtk_text_view_set_wrap_mode")
		reg(&gtkTextViewScrollToMark, gtk, "gtk_text_view_scroll_to_mark")
		reg(&gtkTextViewGetBuffer, gtk, "gtk_text_view_get_buffer")
		reg(&gtkTextBufferSetText, gtk, "gtk_text_buffer_set_text")
		reg(&gtkTextBufferGetStartIter, gtk, "gtk_text_buffer_get_start_iter")
		reg(&gtkTextBufferGetEndIter, gtk, "gtk_text_buffer_get_end_iter")
		reg(&gtkTextBufferInsert, gtk, "gtk_text_buffer_insert")
		reg(&gtkTextBufferCreateMark, gtk, "gtk_text_buffer_create_mark")
		reg(&gtkTextBufferDeleteMark, gtk, "gtk_text_buffer_delete_mark")
		reg(&gtkTextBufferGetCharCount, gtk, "gtk_text_buffer_get_char_count")
		reg(&gtkScrolledWindowNew, gtk, "gtk_scrolled_window_new")
		reg(&gtkScrolledWindowSetPolicy, gtk, "gtk_scrolled_window_set_policy")
		reg(&gtkScrolledWindowSetChild, gtk, "gtk_scrolled_window_set_child")
		reg(&gtkScrolledWindowGetVAdjustment, gtk, "gtk_scrolled_window_get_vadjustment")
		reg(&gtkAdjustmentGetValue, gtk, "gtk_adjustment_get_value")
		reg(&gtkAdjustmentGetUpper, gtk, "gtk_adjustment_get_upper")
		reg(&gtkAdjustmentGetPageSize, gtk, "gtk_adjustment_get_page_size")
		reg(&gtkWidgetSetHExpand, gtk, "gtk_widget_set_hexpand")
		reg(&gtkWidgetSetVExpand, gtk, "gtk_widget_set_vexpand")
		reg(&gtkWidgetSetHalign, gtk, "gtk_widget_set_halign")
		reg(&gtkWidgetSetValign, gtk, "gtk_widget_set_valign")
		reg(&gtkWidgetSetName, gtk, "gtk_widget_set_name")
		reg(&gtkWidgetSetMarginStart, gtk, "gtk_widget_set_margin_start")
		reg(&gtkWidgetSetMarginEnd, gtk, "gtk_widget_set_margin_end")
		reg(&gtkWidgetSetMarginTop, gtk, "gtk_widget_set_margin_top")
		reg(&gtkWidgetSetMarginBottom, gtk, "gtk_widget_set_margin_bottom")
		reg(&gtkWidgetAddCssClass, gtk, "gtk_widget_add_css_class")
		reg(&gtkWidgetSetVisible, gtk, "gtk_widget_set_visible")
		reg(&gtkWidgetGetVisible, gtk, "gtk_widget_get_visible")
		reg(&gtkWidgetHide, gtk, "gtk_widget_hide")
		reg(&gtkFileDialogNew, gtk, "gtk_file_dialog_new")
		reg(&gtkFileDialogSetTitle, gtk, "gtk_file_dialog_set_title")
		reg(&gtkFileDialogSetModal, gtk, "gtk_file_dialog_set_modal")
		reg(&gtkFileDialogOpen, gtk, "gtk_file_dialog_open")
		// gtk_file_dialog_open_finish returns a NULL-terminated array of path
		// strings (char**), not a GFile*. Registered under the real symbol name.
		reg(&gtkFileOpenFinish, gtk, "gtk_file_dialog_open_finish")
	})
	return loadErr
}

// Init loads GTK4 and initialises it, reporting whether a display could be
// opened. An error means the libraries could not be loaded at all (not merely
// that there is no display).
func Init() (ok bool, err error) {
	if err := load(); err != nil {
		return false, err
	}
	return gtkInitCheck(), nil
}

// ---- main loop ---------------------------------------------------------

func MainLoopNew() MainLoop { return MainLoop(gMainLoopNew(0, false)) }
func (l MainLoop) Run()     { gMainLoopRun(uintptr(l)) }
func (l MainLoop) Quit()    { gMainLoopQuit(uintptr(l)) }

// IdleAdd schedules fn to run once on the main loop thread. The primary way to
// marshal work (opened from a godbus goroutine) onto the GTK thread.
func IdleAdd(fn func()) {
	cb := purego.NewCallback(func(_ uintptr) int32 { fn(); return 0 })
	retainCallback(cb)
	gIdleAdd(cb, 0)
}

// TimeoutAdd registers fn to run every ms on the main loop thread, for as long
// as fn returns true (false removes it).
func TimeoutAdd(ms int32, fn func() bool) uint32 {
	cb := purego.NewCallback(func(_ uintptr) int32 {
		if fn() {
			return 1 // G_SOURCE_CONTINUE
		}
		return 0 // G_SOURCE_REMOVE
	})
	retainCallback(cb)
	return gTimeoutAdd(cb, uintptr(ms))
}

func SourceRemove(id uint32) { gSourceRemove(id) }

// ---- signals -----------------------------------------------------------
//
// g_signal_connect_data's C handler is (instance, <signal args…>, data). A
// plain signal (clicked/changed/close-requested) carries no signal args, so the
// handler is (instance, data) = two uintptr. state-set is a no-input-argument,
// boolean-return veto signal: its handler is also (instance, data) = two
// uintptr, returning a gboolean. It fires only on a real state change (user
// gesture or set_active), after the active property is set, so the handler's
// gtkSwitchGetActive already reports the new state.

// OnVoid wires fn to a no-argument, no-veto "clicked"/"changed" style signal.
func (w Widget) OnVoid(signal string, fn func()) uint64 {
	cb := purego.NewCallback(func(_ uintptr, _ uintptr) { fn() })
	retainCallback(cb)
	return gSignalConnectData(uintptr(w), signal, cb, 0, 0, 0)
}

// OnStateSet wires fn to GtkSwitch "state-set". fn receives the NEW state value
// (0/1) read directly from gtkSwitchGetActive: empirically verified on this
// GTK 4.22.5 that the signal fires AFTER the active property is set but before
// the visual state updates, so the handler's get_active() already reports the
// new (post-flip) value. There is no state argument to the signal. It never
// vetoes (returns 0), so GTK applies the flip.
func (w Widget) OnStateSet(fn func(newState int32)) uint64 {
	cb := purego.NewCallback(func(instance uintptr, _ uintptr) int32 {
		n := int32(0)
		if gtkSwitchGetActive(instance) {
			n = 1
		}
		fn(n)
		return 0 // do not veto: let GTK apply the new state
	})
	retainCallback(cb)
	return gSignalConnectData(uintptr(w), "state-set", cb, 0, 0, 0)
}

// OnCloseRequested wires fn to the window "close-request" signal (GTK4's name
// for the GTK3 "close-requested"; GtkWindow emits it when the user requests a
// close, or from gtk_window_close) and vetoes it (returns TRUE) so the window
// is hidden, not freed — a Go uintptr has no ref, and a real destroy would
// leave a dangling handle.
func (w Widget) OnCloseRequested(fn func()) uint64 {
	cb := purego.NewCallback(func(_ uintptr, _ uintptr) int32 {
		fn()
		return 1 // veto: cancel the close
	})
	retainCallback(cb)
	return gSignalConnectData(uintptr(w), "close-request", cb, 0, 0, 0)
}

func WindowNew() Widget { return Widget(gtkWindowNew()) }

// ---- helpers -----------------------------------------------------------

// retainCallback keeps every callback alive for the process: GTK stores the C
// function pointer and may call it at any later time.
var (
	cbMu   sync.Mutex
	cbKeep []uintptr
)

func retainCallback(cb uintptr) {
	cbMu.Lock()
	cbKeep = append(cbKeep, cb)
	cbMu.Unlock()
}

// cstr reads a NUL-terminated C string from p. p==0 yields "". The read is
// bounded so a wild pointer cannot read unbounded memory.
func cstr(p uintptr) string {
	if p == 0 {
		return ""
	}
	const max = 1 << 24 // 16 MiB
	b := (*[max]byte)(unsafe.Pointer(p))
	var i int
	for i < max && b[i] != 0 {
		i++
	}
	return string(b[:i])
}
