package executor

import _ "embed"

// PTYRelayScript is the stdlib-only Python PTY relay helper run as the entry
// command of a terminal session inside the sandbox. It is embedded into the
// binary and materialized into the per-session /tmp (bind-mounted read-only at
// /run/sandboxd) at Start time, so the sandbox image needs nothing beyond
// python3.
//
//go:embed pty_relay.py
var PTYRelayScript []byte
