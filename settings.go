package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings persists to ~/.config/launch-buddy-gnome/settings.json (honoring
// $XDG_CONFIG_HOME). Keys mirror the macOS AppSettings: scriptPath,
// launchAtLogin, restoreOnLaunch.

type Settings struct {
	ScriptPath      string `json:"scriptPath"`
	LaunchAtLogin   bool   `json:"launchAtLogin"`
	RestoreOnLaunch bool   `json:"restoreOnLaunch"`
}

// AppConfigDir is the per-user data directory for settings, the state file and
// the generated brain icons.
func AppConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "launch-buddy-gnome")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "launch-buddy-gnome")
}

func settingsPath() string { return filepath.Join(AppConfigDir(), "settings.json") }
func statePath() string    { return filepath.Join(AppConfigDir(), "state.json") }

// LoadSettings tolerates a missing or corrupt file, falling back to defaults
// for the whole set.
func LoadSettings() Settings {
	s := Settings{ScriptPath: "", LaunchAtLogin: false, RestoreOnLaunch: true}
	raw, err := os.ReadFile(settingsPath())
	if err != nil {
		return s
	}
	var doc struct {
		ScriptPath      *string `json:"scriptPath"`
		LaunchAtLogin   *bool   `json:"launchAtLogin"`
		RestoreOnLaunch *bool   `json:"restoreOnLaunch"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Settings{}
	}
	if doc.ScriptPath != nil {
		s.ScriptPath = *doc.ScriptPath
	}
	if doc.LaunchAtLogin != nil {
		s.LaunchAtLogin = *doc.LaunchAtLogin
	}
	if doc.RestoreOnLaunch != nil {
		s.RestoreOnLaunch = *doc.RestoreOnLaunch
	}
	return s
}

// SaveSettings persists atomically: temp file in the same directory, chmod 0600,
// rename over the destination.
func SaveSettings(s Settings) error {
	dir := AppConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	payload := make([]byte, 0, 128)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, b...)
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after successful rename
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, settingsPath())
}

// LoadRunningState reads state.json ({"running":bool}). Missing/corrupt → false.
func LoadRunningState() bool {
	raw, err := os.ReadFile(statePath())
	if err != nil {
		return false
	}
	var doc struct {
		Running bool `json:"running"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return false
	}
	return doc.Running
}

// WriteRunningState writes the compact {"running":<bool>} state marker.
func WriteRunningState(running bool) error {
	dir := AppConfigDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	payload := []byte(`{"running":` + boolJSON(running) + `}`)
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after successful rename
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.Rename(name, statePath())
}

func boolJSON(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
