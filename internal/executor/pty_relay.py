#!/usr/bin/env python3
"""sandboxd PTY relay helper.

Runs *inside* the workspace sandbox as the entry command of a terminal session.
nsjail cannot allocate a PTY itself, and the sandbox image ships neither socat
nor expect, but Python's stdlib does have `pty`, `termios` and `fcntl` -- so a
small stdlib-only helper is the mechanism.

Interface:

  argv[1]: a JSON object {"shell": "/bin/sh", "cols": N, "rows": M}. The shell
           is exec'd as a session leader on a freshly allocated PTY; the
           process cwd (set by nsjail to the workspace, or a requested cwd) is
           inherited.

  stdin:   a line-delimited JSON control protocol. Each line is one object:
             {"type":"input","data":"<raw bytes>"}   write raw bytes to the PTY
             {"type":"signal","signal":"INT"}         signal the PTY foreground
                                                      process group
             {"type":"resize","cols":N,"rows":M}      TIOCSWINSZ
             {"type":"close"}                         graceful shutdown
           Only the *structured* "type" field is interpreted. The "data" string
           is written to the PTY verbatim, so a client payload that happens to
           look like {"type":"close"} is delivered as literal terminal input,
           never as a control message.

  stdout:  the PTY's output, copied continuously and unbuffered. No framing,
           JSON or sentinel bytes are ever injected, so a client receives the
           exact byte stream a real terminal would produce. The service's
           existing log-following (offset/limit) reads this stream unchanged.

  stderr:  helper diagnostics only (never part of the terminal stream).

The helper exits when the shell exits, and on stdin EOF it exits like a session
leader losing its terminal (SIGHUP, grace, SIGKILL). Stdlib only.
"""
import fcntl
import json
import os
import pty
import select
import signal
import struct
import sys
import termios
import time

# Signals the control protocol may deliver to the PTY foreground process group.
SIGNALS = {
    "INT": signal.SIGINT,
    "TERM": signal.SIGTERM,
    "QUIT": signal.SIGQUIT,
    "HUP": signal.SIGHUP,
    "WINCH": signal.SIGWINCH,
    "KILL": signal.SIGKILL,
    "USR1": signal.SIGUSR1,
    "USR2": signal.SIGUSR2,
    "STOP": signal.SIGSTOP,
    "CONT": signal.SIGCONT,
}

# Bound on unparsed control bytes, so a client streaming data without newlines
# cannot grow the helper's memory without limit.
MAX_CONTROL_BUF = 1 << 20
# Grace between SIGHUP and SIGKILL when tearing the shell down (milliseconds).
SHUTDOWN_GRACE_MS = 3000

# A PTY allocation failure is reported as a distinct, machine-recognisable
# diagnostic on stderr and a dedicated exit code. The executor turns this into a
# failed session with the diagnostic (never a running session whose terminal
# output is a Python traceback). The host /dev/pts and /dev/ptmx mounts are what
# make pty.fork() succeed inside the jail; if they are missing this fires.
PTY_ERROR_PREFIX = "sandboxd-pty-error:"
PTY_ERROR_EXIT = 69  # EX_UNAVAILABLE (sysexits.h); not an nsjail launch code.


def set_winsize(fd, rows, cols):
    try:
        fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    except OSError:
        pass


def foreground_pgrp(master, child):
    """Return the PTY's foreground process group, falling back to the child.

    A client signal must reach the *foreground job* (e.g. a running `sleep`),
    not the shell, so Ctrl-C interrupts the command without ending the session.
    """
    try:
        return os.tcgetpgrp(master)
    except OSError:
        return child


def write_all(fd, data):
    view = memoryview(data)
    while view:
        try:
            n = os.write(fd, view)
        except (BrokenPipeError, OSError):
            return False
        view = view[n:]
    return True


def signal_group(master, child, signo):
    pgid = foreground_pgrp(master, child)
    try:
        os.killpg(pgid, signo)
        return
    except OSError:
        pass
    try:
        os.kill(child, signo)
    except OSError:
        pass


def handle_line(master, child, line):
    if not line.strip():
        return False
    try:
        msg = json.loads(line)
    except ValueError:
        # Not a control message; never inject framing noise into the stream.
        return False
    if not isinstance(msg, dict):
        return False
    typ = msg.get("type")
    if typ == "input":
        data = msg.get("data")
        if isinstance(data, str):
            write_all(master, data.encode("utf-8", "surrogateescape"))
    elif typ == "signal":
        name = msg.get("signal")
        if isinstance(name, str) and name.upper() in SIGNALS:
            signal_group(master, child, SIGNALS[name.upper()])
    elif typ == "resize":
        try:
            cols = int(msg.get("cols") or 0)
            rows = int(msg.get("rows") or 0)
        except (TypeError, ValueError):
            return False
        if cols > 0 and rows > 0:
            set_winsize(master, rows, cols)
    elif typ == "close":
        return True
    return False


def shutdown(child_pid, master):
    """SIGHUP the shell's process group, grace, then SIGKILL."""
    try:
        os.killpg(child_pid, signal.SIGHUP)
    except OSError:
        try:
            os.kill(child_pid, signal.SIGHUP)
        except OSError:
            return
    deadline = time.time() + SHUTDOWN_GRACE_MS / 1000.0
    while time.time() < deadline:
        try:
            pid, _ = os.waitpid(child_pid, os.WNOHANG)
        except ChildProcessError:
            return
        if pid != 0:
            return
        time.sleep(0.02)
    try:
        os.killpg(child_pid, signal.SIGKILL)
    except OSError:
        pass
    try:
        os.waitpid(child_pid, 0)
    except OSError:
        pass


def main():
    cfg = {}
    if len(sys.argv) > 1:
        try:
            cfg = json.loads(sys.argv[1])
        except ValueError:
            cfg = {}
    if not isinstance(cfg, dict):
        cfg = {}
    shell = cfg.get("shell") or "/bin/sh"
    try:
        cols = int(cfg.get("cols") or 80)
        rows = int(cfg.get("rows") or 24)
    except (TypeError, ValueError):
        cols, rows = 80, 24
    cols = cols if cols > 0 else 80
    rows = rows if rows > 0 else 24

    try:
        child, master = pty.fork()
    except OSError as exc:
        # The sandbox has no usable PTY: /dev/ptmx and devpts were not mounted
        # (or are broken). Emit one clean line -- never a traceback -- so the
        # service records a failed session with an actionable diagnostic.
        sys.stderr.write(
            "%s cannot allocate a PTY: %s\n"
            "the sandbox is missing a working /dev/ptmx and devpts\n" % (PTY_ERROR_PREFIX, exc)
        )
        sys.stderr.flush()
        os._exit(PTY_ERROR_EXIT)
    if child == 0:
        # Child: the PTY is already its controlling terminal (pty.fork sets
        # this up), so it is the session leader.
        os.environ.setdefault("TERM", "xterm")
        try:
            os.execv(shell, [shell])
        except OSError as exc:
            sys.stderr.write("pty helper: cannot exec %s: %s\n" % (shell, exc))
            os._exit(127)
        os._exit(127)

    # Parent: relay. Set the initial window size before anything is drawn.
    set_winsize(master, rows, cols)

    stdin_fd = 0
    stdin_open = True
    ctrl_buf = b""
    exit_status = None
    graceful = False

    while True:
        rlist = [master]
        if stdin_open:
            rlist.append(stdin_fd)
        try:
            ready, _, _ = select.select(rlist, [], [], 0.2)
        except InterruptedError:
            ready = []
        except OSError:
            break

        if stdin_open and stdin_fd in ready:
            try:
                chunk = os.read(stdin_fd, 65536)
            except OSError:
                chunk = b""
            if chunk == b"":
                # EOF from the service: exit like a session leader losing its
                # terminal.
                stdin_open = False
                graceful = True
                break
            ctrl_buf += chunk
            if len(ctrl_buf) > MAX_CONTROL_BUF:
                graceful = True
                break
            while b"\n" in ctrl_buf:
                line, ctrl_buf = ctrl_buf.split(b"\n", 1)
                if handle_line(master, child, line):
                    graceful = True
                    stdin_open = False
                    break
            if graceful:
                break

        if master in ready:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            if not write_all(1, data):
                break

        try:
            pid, status = os.waitpid(child, os.WNOHANG)
        except ChildProcessError:
            pid, status = child, 0
        if pid == child:
            exit_status = status
            # Drain any final output the shell produced before exiting.
            drain(master)
            break

    if exit_status is None:
        # The child may have already exited (its PTY reached EOF before we
        # reaped it); reap it if so, otherwise tear it down.
        try:
            wpid, wstatus = os.waitpid(child, os.WNOHANG)
        except ChildProcessError:
            wpid, wstatus = child, 0
        if wpid == child:
            exit_status = wstatus
        else:
            shutdown(child, master)

    if exit_status is None:
        code = 0
    else:
        try:
            code = os.waitstatus_to_exitcode(exit_status)
        except ValueError:
            code = 0
    try:
        sys.stdout.flush()
    except Exception:
        pass
    os._exit(code if code >= 0 else 0)


def drain(master):
    """Read whatever is left on the PTY without blocking."""
    while True:
        try:
            ready, _, _ = select.select([master], [], [], 0.05)
        except OSError:
            return
        if not ready:
            return
        try:
            data = os.read(master, 65536)
        except OSError:
            return
        if not data:
            return
        if not write_all(1, data):
            return


if __name__ == "__main__":
    main()
