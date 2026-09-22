package main

// process.go — start/stop the user script and collect its output. Mirrors the
// macOS ProcessManager:
//
//   * the script is launched as `stdbuf -o0 -e0 <script>` (or
//     `stdbuf -o0 -e0 <shell> <script>` for sh/bash/zsh/csh/ksh/fish) so output
//     is unbuffered; PYTHONUNBUFFERED / PYTHONDONTWRITEBYTECODE are exported;
//   * the child runs in a new session (Setsid == setsid), so its pid is the
//     process-group id and its whole tree lives in that group;
//   * stop() SIGTERMs the group, escalates to SIGKILL after 2 s;
//   * kill_sync() does the same synchronously on quit so no orphans survive;
//   * combined stdout+stderr is kept as a 1000-line rolling buffer.
//
// A Go process can only be reaped once (*exec.Cmd.Wait), so the reader goroutine
// and the terminate path share a single sync.Once. The escalate gate tests
// liveness with Kill(pgid, 0) (child pid == pgid because of Setsid) rather than
// a second Wait.

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	stdoutChunk = 65536
	lineCap     = 1000
	stdBufPath  = "/usr/bin/stdbuf"
)

var shellExts = map[string]bool{
	"sh": true, "bash": true, "zsh": true,
	"csh": true, "ksh": true, "fish": true,
}

type ProcessManager struct {
	onStateChanged func()

	running   bool
	launchErr string // "" when none

	mu    sync.Mutex
	p     *proc // current running command (nil when stopped)
	pgid  int
	lines [lineCap]string
	head  int // next write slot
	count int // lines in the ring (<= lineCap)
	total int // total lines since (re)start
}

// proc bundles a single launched command with its stdout pipe and a fresh
// sync.Once, so each start/stop cycle reaps its child exactly once. A
// manager-level sync.Once cannot be reset and would leak zombies from the
// second start onward.
type proc struct {
	cmd      *exec.Cmd
	out      *os.File // stdout pipe (== stderr)
	waitOnce sync.Once
}

// reap Waits on the command exactly once, whichever of the reader goroutine
// and the terminate path gets there first.
func (p *proc) reap() {
	p.waitOnce.Do(func() {
		if p.cmd != nil {
			_ = p.cmd.Wait()
		}
	})
}

// ring buffer: append a line, evicting the oldest when full.
func (pm *ProcessManager) appendLocked(line string) {
	pm.lines[pm.head] = line
	pm.head = (pm.head + 1) % lineCap
	if pm.count < lineCap {
		pm.count++
	}
	pm.total++
}

// LastLines returns a copy of the current rolling output (oldest first).
func (pm *ProcessManager) LastLines() []string {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	out := make([]string, 0, pm.count)
	start := (pm.head - pm.count + lineCap) % lineCap
	for i := 0; i < pm.count; i++ {
		out = append(out, pm.lines[(start+i)%lineCap])
	}
	return out
}

// TotalLines returns the number of lines emitted since (re)start.
func (pm *ProcessManager) TotalLines() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.total
}

// Pgid returns the live process-group id (0 when not running).
func (pm *ProcessManager) Pgid() int {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.pgid
}

// Running reports whether a script is currently running.
func (pm *ProcessManager) Running() bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.running
}

// LaunchError returns the last start failure, if any.
func (pm *ProcessManager) LaunchError() string {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.launchErr
}

func (pm *ProcessManager) expand(p string) string {
	if len(p) > 1 && p[0] == '~' {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			if p[1] == '/' {
				return home + p[1:]
			}
		}
	}
	return p
}

// Start launches the script at scriptPath, resetting the output buffer.
func (pm *ProcessManager) Start(scriptPath string) {
	pm.mu.Lock()
	pm.launchErr = ""
	path := strings.TrimSpace(scriptPath)
	pm.mu.Unlock()
	if path == "" {
		pm.mu.Lock()
		pm.launchErr = "No script path configured"
		pm.mu.Unlock()
		return
	}
	expanded := pm.expand(path)

	fi, err := os.Stat(expanded)
	if err != nil || !fi.Mode().IsRegular() {
		pm.mu.Lock()
		pm.launchErr = "File not found: " + path
		pm.mu.Unlock()
		return
	}

	// Ensure the script is executable; repair it in place if not.
	mode := fi.Mode().Perm()
	if mode&0o111 == 0 {
		if err := os.Chmod(expanded, mode|0o755); err != nil {
			pm.mu.Lock()
			pm.launchErr = "Permission error: " + err.Error()
			pm.mu.Unlock()
			return
		}
	}

	env := append(os.Environ(),
		"PYTHONUNBUFFERED=1",
		"PYTHONDONTWRITEBYTECODE=1")

	ext := ""
	if i := lastIndexByte(expanded, '.'); i >= 0 {
		ext = strings.ToLower(expanded[i+1:])
	}
	stdbufExists := fileExists(stdBufPath)

	cmdArgs := buildCmd(stdbufExists, shellExts[ext], expanded)

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		pm.mu.Lock()
		pm.launchErr = "Failed to launch: " + err.Error()
		pm.mu.Unlock()
		return
	}

	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = devNull
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		devNull.Close()
		pm.mu.Lock()
		pm.launchErr = "Failed to launch: " + err.Error()
		pm.mu.Unlock()
		return
	}
	// StdoutPipe hands back a concrete *os.File (os.Pipe); cmd.Stderr needs a
	// Writer, and the reader goroutine needs Fd(), so pin the concrete type now.
	stdoutFile := stdout.(*os.File)
	cmd.Stderr = stdoutFile // combine stderr into the same pipe

	if err := cmd.Start(); err != nil {
		devNull.Close()
		pm.mu.Lock()
		pm.launchErr = "Failed to launch: " + err.Error()
		pm.mu.Unlock()
		return
	}
	devNull.Close()

	pr := &proc{cmd: cmd, out: stdoutFile}
	pm.mu.Lock()
	pm.p = pr
	pm.pgid = cmd.Process.Pid // new session leader
	pm.running = true
	pm.mu.Unlock()

	go pm.readOutput(pr)
	pm.notify()
}

func (pm *ProcessManager) notify() {
	pm.mu.Lock()
	cb := pm.onStateChanged
	pm.mu.Unlock()
	if cb != nil {
		cb()
	}
}

func buildCmd(stdbufExists bool, isShell bool, path string) []string {
	shell := "/bin/bash"
	if isShell {
		if fileExists("/bin/zsh") {
			shell = "/bin/zsh"
		}
	}
	if stdbufExists {
		if isShell {
			return []string{stdBufPath, "-o0", "-e0", shell, path}
		}
		return []string{stdBufPath, "-o0", "-e0", path}
	}
	if isShell {
		return []string{shell, path}
	}
	return []string{path}
}

// Stop gracefully stops: SIGTERM the group, escalate to SIGKILL after 2 s.
func (pm *ProcessManager) Stop() {
	p, pgid := pm.detach()
	if p == nil {
		return
	}
	go pm.terminate(p, pgid, 2*time.Second)
}

// KillSync hard-stops synchronously (quit path) so no orphans survive.
func (pm *ProcessManager) KillSync() {
	p, pgid := pm.detach()
	if p == nil {
		return
	}
	pm.terminate(p, pgid, 2*time.Second)
}

// detach clears live state and returns the (proc, pgid) to act on.
func (pm *ProcessManager) detach() (*proc, int) {
	pm.mu.Lock()
	p := pm.p
	pgid := pm.pgid
	pm.p = nil
	pm.pgid = 0
	wasRunning := pm.running
	pm.running = false
	pm.mu.Unlock()
	if wasRunning {
		pm.notify()
	}
	return p, pgid
}

// terminate signals the group and reaps. SIGTERM, wait up to `timeout`, escalate
// to SIGKILL if still alive, wait 5 s, then reap exactly once.
func (pm *ProcessManager) terminate(p *proc, pgid int, timeout time.Duration) {
	signalGroup(pgid, syscall.SIGTERM)
	if !waitForExit(pgid, timeout) {
		signalGroup(pgid, syscall.SIGKILL)
		waitForExit(pgid, 5*time.Second)
	}
	p.reap()
}

// signalGroup sends sig to the whole process group (pgid).
func signalGroup(pgid int, sig syscall.Signal) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, sig)
}

// waitForExit polls Kill(pgid, 0) until the child (pid == pgid) is gone or the
// deadline passes. ESRCH means the child has exited.
func waitForExit(pgid int, timeout time.Duration) bool {
	if pgid <= 0 {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Kill(pgid, 0); err == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readOutput drains the raw pipe fd, lines the output into the ring buffer, and
// (once EOF) reaps the process, reporting an unexpected exit only if we still
// own it (a deliberate stop() has already detached).
func (pm *ProcessManager) readOutput(pr *proc) {
	fd := pr.out.Fd()
	acc := make([]byte, 0, 4096)
	tmp := make([]byte, stdoutChunk)
	for {
		n, err := syscall.Read(int(fd), tmp)
		if n > 0 {
			acc = append(acc, tmp[:n]...)
			for {
				idx := bytesIndexByte(acc, '\n')
				if idx < 0 {
					break
				}
				line := string(acc[:idx])
				acc = acc[idx+1:]
				pm.mu.Lock()
				pm.appendLocked(line)
				pm.mu.Unlock()
			}
		}
		if err != nil || n == 0 {
			break
		}
	}
	// Flush a trailing partial line.
	if len(acc) > 0 {
		pm.mu.Lock()
		pm.appendLocked(string(acc))
		pm.mu.Unlock()
	}
	pr.reap()
	// Only report the unexpected exit if we still own the process.
	pm.mu.Lock()
	wasRunning := false
	if pm.p == pr {
		pm.p = nil
		pm.pgid = 0
		wasRunning = pm.running
		pm.running = false
	}
	pm.mu.Unlock()
	if wasRunning {
		pm.notify()
	}
}

// helpers
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func bytesIndexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}
