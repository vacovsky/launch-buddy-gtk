package main

// main.go — assembly: the app struct (the D-Bus Actions implementation), the
// startup order, and shutdown. Mirrors the reference app.py:
//
//  1. g_set_prgname / g_set_application_name
//  2. resolve the executable path (launch-at-login target + its cwd)
//  3. (re)write the embedded brain icons next to the config, BEFORE restore
//  4. load settings + the persisted running state
//  5. build the process manager, the (lazy) windows, and the D-Bus server
//  6. init GTK (the app is a GUI; no display → fatal, like the reference)
//  7. connect the session bus with the handler, claim org.launchbuddy.Gnome,
//     start the watcher's 2 s registration loop
//  8. restore a previously running script if configured
//  9. run the GTK main loop; on exit: kill the child, persist state,
//     unregister from the StatusNotifierWatcher, release the bus name.
//
// No Gtk.Application is used: GApplication would claim the bus name itself and
// fight this process's godbus service for org.launchbuddy.Gnome.

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"

	dbus "github.com/godbus/dbus/v5"
)

//go:embed icons/brain-stopped.png
var iconBrainStopped []byte

//go:embed icons/brain-running.png
var iconBrainRunning []byte

type app struct {
	pm     *ProcessManager
	s      *Settings
	server *server
	watch  *watcher
	conn   *dbus.Conn

	out     *outputWindow
	setting *settingsWindow

	exePath string
	exeDir  string
	iconDir string
	loop    MainLoop

	shut bool // shutdown ran (quit or post-loop cleanup)
}

// ---- Actions (D-Bus surface; handlers run on godbus goroutines) ------------

func (a *app) ToggleStartStop() {
	if a.pm.Running() {
		a.pm.Stop()
	} else {
		a.pm.Start(a.s.ScriptPath)
	}
	// State-change notification (publishState + state.json) is driven by
	// ProcessManager.notify(), exactly like the reference's _on_state_changed.
}

func (a *app) OpenOutput()   { IdleAdd(a.out.open) }
func (a *app) OpenSettings() { IdleAdd(a.setting.open) }
func (a *app) Quit()         { IdleAdd(a.quit) }

func (a *app) Running() bool { return a.pm.Running() }

func (a *app) ScriptPath() string { return a.s.ScriptPath }

// IconPath is the absolute path of the brain PNG for the given state. The
// extension treats a leading "/" IconName as a file icon (Gio.FileIcon).
func (a *app) IconPath(running bool) string {
	name := "brain-stopped.png"
	if running {
		name = "brain-running.png"
	}
	return filepath.Join(a.iconDir, name)
}

// ---- persistence -----------------------------------------------------------

func (a *app) save() {
	if err := SaveSettings(*a.s); err != nil {
		log.Printf("settings: %v", err)
	}
}

// saveState writes the compact {"running":<bool>} marker.
func (a *app) saveState() {
	if err := WriteRunningState(a.pm.Running()); err != nil {
		log.Printf("state: %v", err)
	}
}

// quit marshalled onto the GTK thread: kill the child synchronously, persist,
// unregister, release the bus name, stop the loop.
func (a *app) quit() {
	a.shutdown()
	a.loop.Quit()
}

// shutdown is idempotent: run it from quit() and again after the loop returns
// (for a loop exit that did not go through quit).
func (a *app) shutdown() {
	if a.shut {
		return
	}
	a.shut = true
	a.pm.KillSync()
	a.saveState()
	a.save()
	a.watch.unregister()
	if a.conn != nil {
		if _, err := a.conn.ReleaseName(busName); err != nil {
			log.Printf("dbus: release %s: %v", busName, err)
		}
		a.conn.Close()
	}
}

func main() {
	// Resolve the GLib/GTK symbols before touching them (Init re-runs load, a
	// no-op via sync.Once, and additionally checks for a display).
	if err := load(); err != nil {
		log.Fatalf("gtk: %v", err)
	}
	gSetPrgname(appID)
	gSetApplicationName(appTitle)

	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("executable: %v", err)
	}
	exeDir := filepath.Dir(exePath)

	iconDir := filepath.Join(AppConfigDir(), "icons")
	if err := os.MkdirAll(iconDir, 0o755); err != nil {
		log.Fatalf("config dir: %v", err)
	}
	writeIcon(iconDir, "brain-stopped.png", iconBrainStopped)
	writeIcon(iconDir, "brain-running.png", iconBrainRunning)

	setting := LoadSettings()
	runtimeRunning := LoadRunningState()

	a := &app{
		pm:      &ProcessManager{},
		s:       &setting,
		exePath: exePath,
		exeDir:  exeDir,
		iconDir: iconDir,
	}
	a.out = newOutputWindow(a)
	a.setting = newSettingsWindow(a)

	ok, err := Init()
	if err != nil {
		log.Fatalf("gtk: %v", err)
	}
	if !ok {
		log.Fatal("gtk: no display available; cannot run the GUI")
	}

	server := &server{actions: a, rev: 1}
	a.server = server

	conn, err := dbus.ConnectSessionBus(dbus.WithHandler(buildHandler(server)))
	if err != nil {
		log.Fatalf("dbus: %v", err)
	}
	server.conn = conn
	a.conn = conn

	reply, err := conn.RequestName(busName, 0)
	if err != nil {
		log.Fatalf("dbus: request name %s: %v", busName, err)
	}
	log.Printf("dbus: RequestName(%s) = %v", busName, reply)

	a.watch = newWatcher(conn)
	go a.watch.run()

	a.loop = MainLoopNew()

	a.pm.onStateChanged = func() {
		a.server.publishState()
		a.saveState()
	}

	if setting.RestoreOnLaunch && runtimeRunning {
		log.Printf("restore: relaunching %q", setting.ScriptPath)
		a.pm.Start(setting.ScriptPath)
	}

	a.loop.Run()
	a.shutdown()
}

func writeIcon(dir, name string, data []byte) {
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		log.Printf("icons: %s: %v", name, err)
	}
}
