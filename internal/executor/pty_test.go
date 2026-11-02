package executor

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sandboxd/internal/model"
)

// TestPTYRelayScriptIsStdlibOnly guards the helper's dependency contract: it
// must not import anything outside the Python standard library, because the
// sandbox image has no third-party packages. It also proves the embedded script
// matches the file on disk.
func TestPTYRelayScriptIsStdlibOnly(t *testing.T) {
	if len(PTYRelayScript) == 0 {
		t.Fatal("embedded PTYRelayScript is empty")
	}
	onDisk, err := os.ReadFile("pty_relay.py")
	if err != nil {
		t.Fatalf("read pty_relay.py: %v", err)
	}
	if string(onDisk) != string(PTYRelayScript) {
		t.Fatal("embedded helper differs from pty_relay.py on disk")
	}
	imports := map[string]bool{
		"fcntl": true, "json": true, "os": true, "pty": true, "select": true,
		"signal": true, "struct": true, "sys": true, "termios": true, "time": true,
	}
	for _, line := range strings.Split(string(PTYRelayScript), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "import ") {
			for _, name := range strings.Split(strings.TrimPrefix(line, "import "), ",") {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				if !imports[name] {
					t.Errorf("helper imports non-stdlib-allowed module %q", name)
				}
			}
		}
	}
}

// ptyHelper drives the embedded helper as a subprocess. It is the harness for
// the real-PTY tests below.
type ptyHelper struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	out    []byte
	done   chan struct{}
}

func startPTYHelper(t *testing.T, shell string, cols, rows int) *ptyHelper {
	t.Helper()
	script := filepath.Join(t.TempDir(), "pty_relay.py")
	if err := os.WriteFile(script, PTYRelayScript, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, _ := json.Marshal(map[string]any{"shell": shell, "cols": cols, "rows": rows})
	cmd := exec.Command("python3", script, string(cfg))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	h := &ptyHelper{t: t, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), done: make(chan struct{})}
	go h.pump()
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
		}
	})
	return h
}

func (h *ptyHelper) pump() {
	defer close(h.done)
	for {
		b, err := h.stdout.ReadByte()
		if err != nil {
			return
		}
		h.mu.Lock()
		h.out = append(h.out, b)
		h.mu.Unlock()
	}
}

func (h *ptyHelper) write(v any) {
	h.t.Helper()
	b, _ := json.Marshal(v)
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, err := h.stdin.Write(append(b, '\n')); err != nil {
		h.t.Fatalf("write control: %v", err)
	}
}

func (h *ptyHelper) input(data string) { h.write(map[string]any{"type": "input", "data": data}) }

func (h *ptyHelper) signal(name string) {
	h.write(map[string]any{"type": "signal", "signal": name})
}

func (h *ptyHelper) resize(cols, rows int) {
	h.write(map[string]any{"type": "resize", "cols": cols, "rows": rows})
}

func (h *ptyHelper) output() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return string(h.out)
}

// waitFor polls the accumulated output until it contains want or the deadline
// elapses.
func (h *ptyHelper) waitFor(want string, timeout time.Duration) bool {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(h.output(), want) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestPTYHelperRealTTY is the executor-level proof (no nsjail required) that
// the relay allocates a real PTY: `test -t 1` succeeds inside the session, and
// state persists between commands in the same session.
func TestPTYHelperRealTTY(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	h := startPTYHelper(t, "/bin/sh", 80, 24)

	h.input("test -t 1 && echo TTY_OK\n")
	if !h.waitFor("TTY_OK", 3*time.Second) {
		t.Fatalf("session did not report a tty; output=%q", h.output())
	}

	h.input("cd /tmp\npwd\n")
	if !h.waitFor("/tmp", 3*time.Second) {
		t.Fatalf("state did not persist; output=%q", h.output())
	}

	h.resize(120, 50)
	h.input("stty size\n")
	if !h.waitFor("50 120", 3*time.Second) {
		t.Fatalf("resize did not reach the PTY; output=%q", h.output())
	}
}

// TestPTYHelperStatePersists proves shell state persists between commands in
// the same session: an alias and a shell function defined in one command are
// usable in the next, and no JSON framing bytes leak into the output stream.
func TestPTYHelperStatePersists(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	h := startPTYHelper(t, "/bin/sh", 80, 24)

	h.input("alias ll='echo ALIAS_OK'\n")
	h.input("ll\n")
	if !h.waitFor("ALIAS_OK", 3*time.Second) {
		t.Fatalf("alias did not persist; output=%q", h.output())
	}
	h.input("greet() { echo FUNC_OK; }\n")
	h.input("greet\n")
	if !h.waitFor("FUNC_OK", 3*time.Second) {
		t.Fatalf("function did not persist; output=%q", h.output())
	}
	// The output stream must be exactly what a terminal produced: no control
	// JSON or sentinels may ever be injected into it.
	out := h.output()
	for _, framing := range []string{`"type":"input"`, `"type":"resize"`, `"type":"close"`} {
		if strings.Contains(out, framing) {
			t.Fatalf("control framing leaked into the output stream: %q found in %q", framing, out)
		}
	}
}

// TestPTYHelperSignalInterruptsForeground proves a signal reaches the
// foreground process group (interrupting a running command) without ending the
// shell: after INT kills `sleep`, the session still runs another command.
func TestPTYHelperSignalInterruptsForeground(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	h := startPTYHelper(t, "/bin/sh", 80, 24)

	h.input("echo READY\n")
	if !h.waitFor("READY", 3*time.Second) {
		t.Fatalf("no shell prompt; output=%q", h.output())
	}
	h.input("sleep 30\n")
	time.Sleep(200 * time.Millisecond)
	h.signal("INT")
	time.Sleep(300 * time.Millisecond)
	h.input("echo AFTER_SIGNAL\n")
	if !h.waitFor("AFTER_SIGNAL", 3*time.Second) {
		t.Fatalf("session did not survive the interrupt; output=%q", h.output())
	}
}

// TestPTYHelperInjectionProof proves client input is written to the PTY as raw
// bytes and can never be interpreted as a control message: a payload that looks
// like {"type":"close"} must arrive as literal text, not close the session.
func TestPTYHelperInjectionProof(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	h := startPTYHelper(t, "/bin/sh", 80, 24)

	// A protocol-shaped string used as a shell comment; if the helper
	// misinterpreted it, the session would end and the echo would not appear.
	h.input("echo INJECT_TEST # {\"type\":\"close\"}\n")
	if !h.waitFor("INJECT_TEST", 3*time.Second) {
		t.Fatalf("injection-shaped input confused the session; output=%q", h.output())
	}
	// Still alive after the injection attempt.
	h.input("echo STILL_ALIVE\n")
	if !h.waitFor("STILL_ALIVE", 3*time.Second) {
		t.Fatalf("session ended after protocol-shaped input; output=%q", h.output())
	}
}

// TestPTYHelperCloseEndsSession proves the structured close message ends the
// helper cleanly.
func TestPTYHelperCloseEndsSession(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	h := startPTYHelper(t, "/bin/sh", 80, 24)
	h.input("echo BEFORE\n")
	if !h.waitFor("BEFORE", 3*time.Second) {
		t.Fatalf("no output; %q", h.output())
	}
	h.write(map[string]any{"type": "close"})
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not exit after close")
	}
}

// TestPTYRelayScriptHandlesAllocationFailure guards the loud-failure contract
// of the helper: it must catch a PTY allocation error, emit the machine-readable
// PTYErrorMarker (never a traceback), and exit with the dedicated code.
func TestPTYRelayScriptHandlesAllocationFailure(t *testing.T) {
	s := string(PTYRelayScript)
	for _, want := range []string{
		`PTY_ERROR_PREFIX = "pty-error:"`,
		"except OSError as exc:",
		"os._exit(PTY_ERROR_EXIT)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("helper must handle PTY allocation failure; missing %q", want)
		}
	}
}

// TestPTYFailureDetectedFromHelperStderr proves the executor turns the helper's
// marker into a SandboxError and does not misfire on a healthy session.
func TestPTYFailureDetectedFromHelperStderr(t *testing.T) {
	tail := newTailBuffer(1024)
	_, _ = tail.Write([]byte("noise\npty-error: cannot allocate a PTY: out of pty devices\n" +
		"the sandbox is missing a working /dev/ptmx and devpts\n"))
	err := ptyFailure(tail, 69)
	if err == nil {
		t.Fatal("expected a PTY failure")
	}
	if !strings.Contains(err.Error(), "PTY allocation failed") || !strings.Contains(err.Error(), "out of pty devices") {
		t.Fatalf("diagnostic = %q", err.Error())
	}
	// A clean exit is never a failure, even if the marker were present.
	if ptyFailure(tail, 0) != nil {
		t.Error("exit 0 must not be a PTY failure")
	}
	// A tail without the marker (an ordinary shell exit) is not a PTY failure.
	clean := newTailBuffer(64)
	_, _ = clean.Write([]byte("$ exit 2\n"))
	if ptyFailure(clean, 2) != nil {
		t.Error("a shell that exits 2 without the marker must not be a PTY failure")
	}
	if ptyFailure(nil, 69) != nil {
		t.Error("a non-PTY process has no tail and must not be reclassified")
	}
}

// TestRenderSessionConfigMountsHostDevpts proves a PTY session's rendered config
// binds the host /dev/pts and /dev/ptmx read-only. This is the measured minimum
// that lets pty.fork() succeed inside the jail; without it the relay dies with
// "out of pty devices".
func TestRenderSessionConfigMountsHostDevpts(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{ID: "ws_test", Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/wsroot",
		Cwd:           "/",
		PTY:           true,
		Argv:          []string{"/usr/bin/python3", "-c", "pass", `{"shell":"/bin/sh","cols":80,"rows":24}`},
	}, "/tmp/session-tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	for _, dev := range []string{"/dev/pts", "/dev/ptmx"} {
		mount := `src: "` + dev + `"
  dst: "` + dev + `"
  is_bind: true
  rw: false`
		if !strings.Contains(out, mount) {
			t.Errorf("a PTY session must bind-mount the host %s read-only:\n%s", dev, out)
		}
	}
}

// TestRenderConfigOmitsDevptsForOneShotExec proves a one-shot exec renders no
// pty devices: the mount is added only for a session, so ordinary execution
// keeps the narrower /dev (null/urandom/zero only).
func TestRenderConfigOmitsDevptsForOneShotExec(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	if strings.Contains(out, "/dev/pts") || strings.Contains(out, "/dev/ptmx") {
		t.Errorf("a one-shot exec must not get pty devices:\n%s", out)
	}
	for _, dev := range []string{"/dev/null", "/dev/urandom", "/dev/zero"} {
		if !strings.Contains(out, `src: "`+dev+`"`) {
			t.Errorf("one-shot exec must still mount %s:\n%s", dev, out)
		}
	}
}

// TestRenderSessionConfigHelperNoFile proves the PTY relay source is passed to
// the interpreter inline (`python3 -c "<source>"`) and that NO helper file or
// fixture directory is mounted into the sandbox. The engine's relay source must
// not exist as a readable file inside the jail, so `/run/sandboxd` (and every
// engine-named path) is absent from the rendered config while the relay is
// still the entry command. This is the unprivileged-environment assertion for
// the session sandbox change (the real jail needs privileges).
func TestRenderSessionConfigHelperNoFile(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{ID: "ws_test", Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderSessionConfig(Request{
		Workspace:     ws,
		WorkspaceRoot: "/tmp/wsroot",
		Cwd:           "/",
		PTY:           true,
		Shell:         "/bin/sh",
		Cols:          80,
		Rows:          24,
	}, "/tmp/session-tmp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	out := readFile(t, path)

	if strings.Contains(out, "/run/sandboxd") {
		t.Errorf("the relay must not be mounted from a fixture directory:\n%s", out)
	}
	if strings.Contains(out, "pty_relay") {
		t.Errorf("the relay must not be named as a file inside the jail:\n%s", out)
	}
	// The entry command is python3 with the inline source as the argument to -c.
	if !strings.Contains(out, `arg: "-c"`) {
		t.Errorf("the relay must be passed inline via -c:\n%s", out)
	}
	if !strings.Contains(out, string(PTYRelayScript)) && !strings.Contains(out, `import pty`) {
		t.Errorf("the relay source must be the -c argument:\n%s", out)
	}
	if !strings.Contains(out, `arg: "{\"shell\":\"/bin/sh\",\"cols\":80,\"rows\":24}"`) {
		t.Errorf("helper argv must carry shell and size:\n%s", out)
	}
	// The session must keep the full workload isolation: seccomp, uidmap, no
	// proc, private net unless allow_net.
	for _, want := range []string{"seccomp_string:", "uidmap {", "mount_proc: false", "clone_newnet: true"} {
		if !strings.Contains(out, want) {
			t.Errorf("session config missing isolation setting %q:\n%s", want, out)
		}
	}
}

// TestRenderSessionConfigNoHelperMount proves a non-session request renders no
// /run/sandboxd mount, so the change is additive.
func TestRenderSessionConfigNoHelperMount(t *testing.T) {
	r := NewNsjailRunner(testCfg(), writeTemplate(t), t.TempDir())
	ws := &model.Workspace{Limits: model.Limits{CPUCores: 1, MemoryMB: 128, DiskMB: 1, MaxWallTimeSec: 5, MaxRuns: 1}}
	path, err := r.renderConfig(Request{Workspace: ws, WorkspaceRoot: "/tmp/x", Argv: []string{"/bin/true"}}, "/tmp/rt")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if strings.Contains(readFile(t, path), "/run/sandboxd") {
		t.Error("non-session config must not mount /run/sandboxd")
	}
}
