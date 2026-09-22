package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Launch-at-login via a user autostart .desktop entry:
// ~/.config/autostart/launch-buddy-gnome.desktop (honoring $XDG_CONFIG_HOME).

const desktopTemplate = `[Desktop Entry]
Type=Application
Name=Launch Buddy
Comment=Start at login (managed by Launch Buddy)
Exec=%s
Path=%s
X-GNOME-Autostart=true
X-GNOME-Autostart-Distraction=false
Terminal=false
`

func desktopPath() string {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, _ := os.UserHomeDir()
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "autostart", "launch-buddy-gnome.desktop")
}

// SetAutostart installs the .desktop entry (enabled) or removes it (disabled).
func SetAutostart(enabled bool, execPath, cwd string) error {
	path := desktopPath()
	if enabled {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf(desktopTemplate, execPath, cwd)), 0o644); err != nil {
			return err
		}
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
