package main

// dbus.go — the SNI + dbusmenu + watcher layer over the user session bus,
// built on godbus/dbus/v5. godbus serializes the (ia{sv}av) GetLayout reply and
// the a(ia{sv}) GetGroupProperties reply natively — the exact shapes the C
// dbus-python binding could not emit.
//
// Served on bus name org.launchbuddy.Gnome:
//   * org.kde.StatusNotifierItem  @ /StatusNotifierItem
//   * com.canonical.dbusmenu      @ /org/launchbuddy/Menu
//
// consumed read-only by the appindicatorsupport shell extension. The extension
// builds its proxies by calling our Introspect and parsing the XML we serve, so
// this file is the single source of truth for the wire contract.

import (
	_ "embed"
	"log"
	"sync"
	"time"

	dbus "github.com/godbus/dbus/v5"
)

const (
	busName  = "org.launchbuddy.Gnome"
	menuPath = "/org/launchbuddy/Menu"
	sniPath  = "/StatusNotifierItem"
	appID    = "launch-buddy-gnome"
	appTitle = "Launch Buddy"

	propsIface      = "org.freedesktop.DBus.Properties"
	menuIface       = "com.canonical.dbusmenu"
	sniIface        = "org.kde.StatusNotifierItem"
	watcherIface    = "org.kde.StatusNotifierWatcher"
	introspectIface = "org.freedesktop.DBus.Introspectable"

	mStart    = 1
	mSep      = 2
	mOutput   = 3
	mSettings = 4
	mQuit     = 5
)

// The extension's exact introspection XML, served from Introspect() so a
// new_for_bus_sync client builds the same gInterfaceInfo the extension's
// new_for_xml path builds.
//
//go:embed interfaces-xml/DBusMenu.xml
var xmlDBusMenu string

//go:embed interfaces-xml/StatusNotifierItem.xml
var xmlSNI string

//go:embed interfaces-xml/Properties.xml
var xmlProps string

// Actions is the set of operations the D-Bus handlers drive. Implemented by the
// app (main.go). The D-Bus handlers run on godbus goroutines, so Actions must
// marshal UI work onto the GTK main thread itself; process-manager operations
// are goroutine-safe and need no marshaling.
type Actions interface {
	ToggleStartStop()
	OpenOutput()
	OpenSettings()
	Quit()
	// Live state accessors for the property getters.
	Running() bool
	ScriptPath() string
	IconPath(running bool) string
}

// menuNode is one node of the canonical dbusmenu tree. Exported fields map
// exactly to the (ia{sv}av) struct: ID (i), Properties (a{sv}), Children (av).
type menuNode struct {
	ID         int32
	Properties map[string]interface{}
	Children   []dbus.Variant
}

// groupProps is one element of the a(ia{sv}) GetGroupProperties reply:
// (id, properties) with NO children.
type groupProps struct {
	ID         int32
	Properties map[string]interface{}
}

// menuEvent is one element of the a(isvu) EventGroup in-arg: (id, event, data,
// timestamp).
type menuEvent struct {
	ID    int32
	Event string
	Data  dbus.Variant
	Time  uint32
}

// pixmap is one element of the a(iiay) SNI pixmap array: (width, height, image).
// image is a byte string per the SNI spec (y = uint8); always empty here.
type pixmap struct {
	Width  int32
	Height int32
	Pixmap []byte
}

// sniToolTip is the (sa(iiay)ss) SNI ToolTip struct: (value, iconPixmaps,
// iconPath, status, description). iconPixmaps is always nil (an empty a(iiay))
// — the extension never reads ToolTip (it is commented out of the SNI XML we
// serve), so this is mirror-correctness only.
type sniToolTip struct {
	Value    string
	Pixmaps  []pixmap
	IconPath string
	Status   string
	Detail   string
}

// method implements dbus.Method with a hand-rolled decode and a Go closure.
// godbus's decodeArguments prefers the method's DecodeArguments when present,
// so the `decode` closure (decodeN) is the path that actually runs.
type method struct {
	in  []any
	out []any

	decode func(m *dbus.Message, in []any) []any
	call   func(args ...any) ([]any, error)
}

func (m method) NumArguments() int               { return len(m.in) }
func (m method) NumReturns() int                 { return len(m.out) }
func (m method) ArgumentValue(i int) any         { return m.in[i] }
func (m method) ReturnValue(i int) any           { return m.out[i] }
func (m method) Call(args ...any) ([]any, error) { return m.call(args...) }
func (m method) DecodeArguments(c *dbus.Conn, sender string, msg *dbus.Message, body []any) ([]any, error) {
	return m.decode(msg, nil), nil
}

// iface implements dbus.Interface.
type iface struct {
	methods map[string]dbus.Method
}

func (i *iface) LookupMethod(name string) (dbus.Method, bool) {
	m, ok := i.methods[name]
	return m, ok
}

// object implements dbus.ServerObject.
type object struct {
	ifaces map[string]dbus.Interface
}

func (o *object) LookupInterface(name string) (dbus.Interface, bool) {
	i, ok := o.ifaces[name]
	return i, ok
}

// handler implements dbus.Handler.
type handler struct {
	objects map[dbus.ObjectPath]dbus.ServerObject
}

func (h *handler) LookupObject(path dbus.ObjectPath) (dbus.ServerObject, bool) {
	o, ok := h.objects[path]
	return o, ok
}

// decodeN returns the message body iff it has exactly n elements, else nil.
func decodeN(n int) func(m *dbus.Message, in []any) []any {
	return func(m *dbus.Message, _ []any) []any {
		if len(m.Body) != n {
			return nil
		}
		return m.Body
	}
}

// server is the SNI item; the menu methods live on it too (both read the app's
// live state through Actions). `fillState` is the cached icon state, updated by
// publishState; the SNI icon properties read it while the ToolTip status word
// reads the live Running() (mirroring the reference).
type server struct {
	conn    *dbus.Conn
	actions Actions
	rev     uint32 // constant 1; the reference never bumps it or emits signals

	mu        sync.Mutex
	fillState bool
}

// fill reads the cached icon state.
func (s *server) fill() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fillState
}

// publishState swaps the panel icon (hollow<->filled) via a plain-string
// IconName PropertiesChanged. The extension's _onPropertiesChanged treats any
// Icon* change as an icon update and reloads the file.
func (s *server) publishState() {
	running := s.actions.Running()
	s.mu.Lock()
	s.fillState = running
	s.mu.Unlock()
	err := s.conn.Emit(dbus.ObjectPath(sniPath), propsIface+".PropertiesChanged",
		sniIface,
		map[string]dbus.Variant{"IconName": dbus.MakeVariant(s.actions.IconPath(running))},
		[]string{})
	if err != nil {
		log.Printf("tray: emit PropertiesChanged: %v", err)
	}
}

func node(id int32, props map[string]interface{}, kids ...*menuNode) *menuNode {
	n := &menuNode{ID: id, Properties: props}
	for _, k := range kids {
		n.Children = append(n.Children, dbus.MakeVariant(k))
	}
	return n
}

// itemProps returns the current property map for a menu item id ({} for an
// unknown id), reading live state.
func (s *server) itemProps(id int32) map[string]interface{} {
	running := s.actions.Running()
	switch id {
	case mStart:
		label, state := "Start Script", int32(0)
		if running {
			label, state = "Stop Script", 1
		}
		return map[string]interface{}{
			"label": label, "type": "standard", "visible": true,
			"enabled":     s.actions.ScriptPath() != "",
			"toggle-type": "checkmark", "toggle-state": state,
		}
	case mSep:
		return map[string]interface{}{"type": "separator", "visible": true, "enabled": true}
	case mOutput:
		return map[string]interface{}{"label": "View Output", "type": "standard", "visible": true, "enabled": true}
	case mSettings:
		return map[string]interface{}{"label": "Settings…", "type": "standard", "visible": true, "enabled": true}
	case mQuit:
		return map[string]interface{}{"label": "Quit", "type": "standard", "visible": true, "enabled": true}
	}
	return map[string]interface{}{}
}

// tree returns the full dbusmenu tree (root + 5 leaves), with FULL properties on
// every item so the panel never races a follow-up GetGroupProperties.
func (s *server) tree() *menuNode {
	return node(0, map[string]interface{}{
		"label": "root", "visible": true, "enabled": true,
		"type": "root", "children-display": "submenu",
	},
		node(mStart, s.itemProps(mStart)),
		node(mSep, s.itemProps(mSep)),
		node(mOutput, s.itemProps(mOutput)),
		node(mSettings, s.itemProps(mSettings)),
		node(mQuit, s.itemProps(mQuit)),
	)
}

// --- com.canonical.dbusmenu ---------------------------------------------

// getLayout mirrors the reference: parentId != 0 -> an empty leaf (never an
// error); propertyNames and recursionDepth are ignored (always the full tree
// with full properties).
func (s *server) getLayout(args ...any) ([]any, error) {
	parentID, _ := args[0].(int32)
	if parentID != 0 {
		return []any{s.rev, &menuNode{ID: parentID, Properties: map[string]interface{}{}}}, nil
	}
	return []any{s.rev, s.tree()}, nil
}

// getProperty returns a single item property, wrapped by its type (label/
// visible/enabled/type/... as s, toggle-state as i), "Version" as u32 3, else "".
func (s *server) getProperty(args ...any) ([]any, error) {
	id, _ := args[0].(int32)
	name, _ := args[1].(string)
	p := s.itemProps(id)
	if v, ok := p[name]; ok {
		return []any{dbus.MakeVariant(v)}, nil
	}
	if name == "Version" {
		return []any{dbus.MakeVariant(uint32(3))}, nil
	}
	return []any{dbus.MakeVariant("")}, nil
}

// getGroupProperties returns one row per requested id, in order (unknown id ->
// empty props); properties are filtered to `names` only when names is
// non-empty. The extension always calls it with names=[].
func (s *server) getGroupProperties(args ...any) ([]any, error) {
	ids, _ := args[0].([]int32)
	names, _ := args[1].([]string)
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := make([]*groupProps, 0, len(ids))
	for _, id := range ids {
		props := s.itemProps(id)
		if len(want) > 0 {
			filtered := map[string]interface{}{}
			for k, v := range props {
				if want[k] {
					filtered[k] = v
				}
			}
			props = filtered
		}
		out = append(out, &groupProps{ID: id, Properties: props})
	}
	return []any{out}, nil
}

// dispatchEvent handles one "clicked" event by menu id.
func (s *server) dispatchEvent(id int32, event string) {
	if event != "clicked" {
		return
	}
	switch id {
	case mStart:
		s.actions.ToggleStartStop()
	case mOutput:
		s.actions.OpenOutput()
	case mSettings:
		s.actions.OpenSettings()
	case mQuit:
		s.actions.Quit()
	}
}

func (s *server) event(args ...any) ([]any, error) {
	id, _ := args[0].(int32)
	ev, _ := args[1].(string)
	s.dispatchEvent(id, ev)
	return nil, nil
}

func (s *server) eventGroup(args ...any) ([]any, error) {
	events, _ := args[0].([]menuEvent)
	for _, e := range events {
		s.dispatchEvent(e.ID, e.Event)
	}
	return []any{[]int32{}}, nil
}

func (s *server) menuMethods() map[string]dbus.Method {
	return map[string]dbus.Method{
		"GetLayout": method{
			in:     []any{int32(0), int32(0), []string{}},
			out:    []any{uint32(0), &menuNode{}},
			decode: decodeN(3),
			call:   func(args ...any) ([]any, error) { return s.getLayout(args...) },
		},
		"GetGroupProperties": method{
			in:     []any{[]int32{}, []string{}},
			out:    []any{[]*groupProps{}},
			decode: decodeN(2),
			call:   func(args ...any) ([]any, error) { return s.getGroupProperties(args...) },
		},
		"GetProperty": method{
			in:     []any{int32(0), "s"},
			out:    []any{dbus.Variant{}},
			decode: decodeN(2),
			call:   func(args ...any) ([]any, error) { return s.getProperty(args...) },
		},
		"Event": method{
			in:     []any{int32(0), "s", dbus.Variant{}, uint32(0)},
			decode: decodeN(4),
			call:   func(args ...any) ([]any, error) { return s.event(args...) },
		},
		"EventGroup": method{
			in:     []any{[]menuEvent{}},
			out:    []any{[]int32{}},
			decode: decodeN(1),
			call:   func(args ...any) ([]any, error) { return s.eventGroup(args...) },
		},
		"AboutToShow": method{
			in:     []any{int32(0)},
			out:    []any{bool(true)},
			decode: decodeN(1),
			call:   func(args ...any) ([]any, error) { return []any{true}, nil },
		},
		"AboutToShowRecursive": method{
			in:     []any{int32(0)},
			out:    []any{bool(true)},
			decode: decodeN(1),
			call:   func(args ...any) ([]any, error) { return []any{true}, nil },
		},
	}
}

// --- org.kde.StatusNotifierItem -----------------------------------------

// sniProps returns all 16 SNI properties as typed variants. IconName and the
// ToolTip icon path read the cached `fill`; the ToolTip status word reads the
// live Running(). Status is the constant "Active".
func (s *server) sniProps() map[string]dbus.Variant {
	running := s.actions.Running()
	fill := s.fill()
	icon := s.actions.IconPath(fill)
	status := "Stopped"
	if running {
		status = "Running"
	}
	return map[string]dbus.Variant{
		"Id":                  dbus.MakeVariant(appID),
		"Category":            dbus.MakeVariant("ApplicationStatus"),
		"Status":              dbus.MakeVariant("Active"),
		"Title":               dbus.MakeVariant(appTitle),
		"WindowId":            dbus.MakeVariant(int32(0)),
		"IconName":            dbus.MakeVariant(icon),
		"IconThemePath":       dbus.MakeVariant(""),
		"IconPixmap":          dbus.MakeVariant([]pixmap{}),
		"OverlayIconName":     dbus.MakeVariant(""),
		"OverlayIconPixmap":   dbus.MakeVariant([]pixmap{}),
		"AttentionIconName":   dbus.MakeVariant(""),
		"AttentionIconPixmap": dbus.MakeVariant([]pixmap{}),
		"AttentionMovieName":  dbus.MakeVariant(""),
		"ItemIsMenu":          dbus.MakeVariant(false),
		"ToolTip":             dbus.MakeVariant(sniToolTip{appTitle, nil, icon, status, "Toggle script"}),
		"Menu":                dbus.MakeVariant(dbus.ObjectPath(menuPath)),
	}
}

func (s *server) sniMethods() map[string]dbus.Method {
	ii := []any{int32(0), int32(0)}
	return map[string]dbus.Method{
		"Activate": method{
			in:     ii,
			decode: decodeN(2),
			call:   func(args ...any) ([]any, error) { s.actions.OpenOutput(); return nil, nil },
		},
		"SecondaryActivate": method{
			in:     ii,
			decode: decodeN(2),
			call:   func(args ...any) ([]any, error) { s.actions.ToggleStartStop(); return nil, nil },
		},
		// XAyatanaSecondaryActivate is declared in the SNI XML we serve, so the
		// extension sets _hasAyatanaSecondaryActivate=true; implement it so it
		// can never receive UnknownMethod. Same behavior as SecondaryActivate.
		"XAyatanaSecondaryActivate": method{
			in:     []any{uint32(0)},
			decode: decodeN(1),
			call:   func(args ...any) ([]any, error) { s.actions.ToggleStartStop(); return nil, nil },
		},
		"Scroll": method{
			in:     []any{int32(0), "s"},
			decode: decodeN(2),
			call:   func(args ...any) ([]any, error) { return nil, nil },
		},
		"ContextMenu": method{
			// Our SNI XML declares ContextMenu(x i, y i) = ii; clients generated
			// from our introspection always send exactly that.
			in:     ii,
			decode: decodeN(2),
			call:   func(args ...any) ([]any, error) { return nil, nil },
		},
		"ProvideXdgActivationToken": method{
			in:     []any{"s"},
			decode: decodeN(1),
			call:   func(args ...any) ([]any, error) { return nil, nil },
		},
	}
}

// --- org.freedesktop.DBus.Properties (menu) -----------------------------

func (s *server) menuGet(ifaceName, name string) (dbus.Variant, error) {
	if ifaceName != menuIface {
		return dbus.Variant{}, dbus.NewError(propsIface+".UnknownInterface", []any{})
	}
	switch name {
	case "Version":
		return dbus.MakeVariant(uint32(3)), nil
	case "TextDirection":
		return dbus.MakeVariant("ltr"), nil
	case "Status":
		return dbus.MakeVariant("enabled"), nil
	case "IconThemePath":
		return dbus.MakeVariant([]string{}), nil
	}
	return dbus.Variant{}, dbus.NewError(propsIface+".UnknownProperty", []any{})
}

func (s *server) menuGetAll(ifaceName string) map[string]dbus.Variant {
	if ifaceName != menuIface {
		return map[string]dbus.Variant{}
	}
	return map[string]dbus.Variant{
		"Version":       dbus.MakeVariant(uint32(3)),
		"TextDirection": dbus.MakeVariant("ltr"),
		"Status":        dbus.MakeVariant("enabled"),
		"IconThemePath": dbus.MakeVariant([]string{}),
	}
}

func (s *server) menuPropsMethods() map[string]dbus.Method {
	return map[string]dbus.Method{
		"Get": method{
			in:     []any{"s", "s"},
			out:    []any{dbus.Variant{}},
			decode: decodeN(2),
			call: func(args ...any) ([]any, error) {
				ifaceName, _ := args[0].(string)
				name, _ := args[1].(string)
				v, err := s.menuGet(ifaceName, name)
				if err != nil {
					return nil, err
				}
				return []any{v}, nil
			},
		},
		"GetAll": method{
			in:     []any{"s"},
			out:    []any{map[string]dbus.Variant{}},
			decode: decodeN(1),
			call: func(args ...any) ([]any, error) {
				ifaceName, _ := args[0].(string)
				return []any{s.menuGetAll(ifaceName)}, nil
			},
		},
	}
}

// --- org.freedesktop.DBus.Properties (SNI) ------------------------------

func (s *server) sniGet(ifaceName, name string) (dbus.Variant, error) {
	if ifaceName != sniIface {
		return dbus.Variant{}, dbus.NewError(propsIface+".UnknownInterface", []any{})
	}
	p := s.sniProps()
	v, ok := p[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError(propsIface+".UnknownProperty", []any{})
	}
	return v, nil
}

func (s *server) sniPropsMethods() map[string]dbus.Method {
	return map[string]dbus.Method{
		"Get": method{
			in:     []any{"s", "s"},
			out:    []any{dbus.Variant{}},
			decode: decodeN(2),
			call: func(args ...any) ([]any, error) {
				ifaceName, _ := args[0].(string)
				name, _ := args[1].(string)
				v, err := s.sniGet(ifaceName, name)
				if err != nil {
					return nil, err
				}
				return []any{v}, nil
			},
		},
		"GetAll": method{
			in:     []any{"s"},
			out:    []any{map[string]dbus.Variant{}},
			decode: decodeN(1),
			call: func(args ...any) ([]any, error) {
				ifaceName, _ := args[0].(string)
				if ifaceName != sniIface {
					return []any{map[string]dbus.Variant{}}, nil
				}
				return []any{s.sniProps()}, nil
			},
		},
		"Set": method{
			in:     []any{"s", "s", dbus.Variant{}},
			decode: decodeN(3),
			call:   func(args ...any) ([]any, error) { return nil, nil },
		},
	}
}

// --- org.freedesktop.DBus.Introspectable --------------------------------

func (s *server) introspectMethods(path string) map[string]dbus.Method {
	var xml string
	switch path {
	case menuPath:
		xml = "<node>" + xmlDBusMenu + xmlProps + "</node>"
	case sniPath:
		xml = "<node>" + xmlSNI + xmlProps + "</node>"
	default:
		xml = "<node/>"
	}
	return map[string]dbus.Method{
		"Introspect": method{
			in:     []any{},
			out:    []any{""},
			decode: decodeN(0),
			call:   func(args ...any) ([]any, error) { return []any{xml}, nil },
		},
	}
}

// buildHandler assembles the object tree: each object has its OWN Properties
// iface instance (menu vs SNI — different Get/GetAll behavior), the primary
// interface, and the introspectable.
func buildHandler(s *server) dbus.Handler {
	return &handler{objects: map[dbus.ObjectPath]dbus.ServerObject{
		dbus.ObjectPath(menuPath): &object{ifaces: map[string]dbus.Interface{
			menuIface:       &iface{methods: s.menuMethods()},
			propsIface:      &iface{methods: s.menuPropsMethods()},
			introspectIface: &iface{methods: s.introspectMethods(menuPath)},
		}},
		dbus.ObjectPath(sniPath): &object{ifaces: map[string]dbus.Interface{
			sniIface:        &iface{methods: s.sniMethods()},
			propsIface:      &iface{methods: s.sniPropsMethods()},
			introspectIface: &iface{methods: s.introspectMethods(sniPath)},
		}},
	}}
}

// watcher owns the StatusNotifierWatcher registration and the 2 s watchdog.
// It both retries an initial registration (mirroring the reference's
// GLib.timeout_add(2000, _watchdog), which never returns False) and self-heals
// after success: the extension can silently drop a registered item (a
// name-owner flap, a proxy-init failure, an extension reload), and a success
// reply gives no way to detect that. So every tick it verifies our item is
// still listed in the watcher's RegisteredStatusNotifierItems and, if not,
// re-registers. Re-registration is idempotent: the extension resets an item
// it already tracks, which also rebuilds a panel icon that was dropped.
type watcher struct {
	conn *dbus.Conn

	mu         sync.Mutex
	registered bool
}

func newWatcher(conn *dbus.Conn) *watcher {
	return &watcher{conn: conn}
}

// nameOwned reports whether `name` is currently owned on the bus (any client),
// per org.freedesktop.DBus.ListNames.
func (w *watcher) nameOwned(name string) bool {
	var list []string
	err := w.conn.BusObject().Call("org.freedesktop.DBus.ListNames", 0).Store(&list)
	if err != nil {
		return false
	}
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// register calls RegisterStatusNotifierItem on the watcher. The bus name is
// the argument form this extension accepts (non-path, matches its
// BUS_ADDRESS_REGEX); it resolves it to our unique name and uses the default
// /StatusNotifierItem object path.
func (w *watcher) register() bool {
	err := w.conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").
		Call(watcherIface+".RegisterStatusNotifierItem", 0, busName).Err
	if err != nil {
		log.Printf("watcher: registration failed (will retry): %v", err)
		return false
	}
	w.mu.Lock()
	first := !w.registered
	w.registered = true
	w.mu.Unlock()
	if first {
		log.Printf("watcher: registered with StatusNotifierWatcher (%s)", busName)
	}
	return true
}

// listContainsItem reports whether the watcher's RegisteredStatusNotifierItems
// still lists our item. On an error (watcher briefly unreachable) it returns
// true so the self-heal does not flap on transient glitches.
func (w *watcher) listContainsItem() bool {
	var items []string
	err := w.conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").
		Call("org.freedesktop.DBus.Properties.Get", 0,
			watcherIface, "RegisteredStatusNotifierItems").Store(&items)
	if err != nil {
		return true
	}
	for _, it := range items {
		// Our indicatorId is exactly the bus name: the extension omits the
		// default /StatusNotifierItem object path from the id.
		if it == busName {
			return true
		}
	}
	return false
}

func (w *watcher) tryRegister() {
	if !w.nameOwned("org.kde.StatusNotifierWatcher") {
		// Watcher gone (shell restart or extension reload): forget the
		// registration so a new watcher instance picks us up.
		w.mu.Lock()
		w.registered = false
		w.mu.Unlock()
		return
	}
	// Only register once we own our own bus name; the watcher would otherwise
	// fetch properties from a name nobody serves. conn.Names() lists the names
	// this connection owns.
	ownsName := false
	for _, n := range w.conn.Names() {
		if n == busName {
			ownsName = true
			break
		}
	}
	if !ownsName {
		return
	}
	if w.registeredNow() {
		if w.listContainsItem() {
			return
		}
		log.Printf("watcher: item lost from watcher list; re-registering")
	}
	w.register()
}

func (w *watcher) registeredNow() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.registered
}

func (w *watcher) run() {
	w.tryRegister()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		w.tryRegister()
	}
}

func (w *watcher) unregister() {
	w.mu.Lock()
	registered := w.registered
	w.registered = false
	w.mu.Unlock()
	if !registered {
		return
	}
	err := w.conn.Object("org.kde.StatusNotifierWatcher", "/StatusNotifierWatcher").
		Call(watcherIface+".UnregisterStatusNotifierItem", 0, busName).Err
	if err != nil {
		log.Printf("watcher: unregister failed (ok on quit): %v", err)
	}
}
