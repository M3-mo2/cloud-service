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

// HostMaskShim is the compiled host-identity masking shim (internal/executor/
// hostmask/hostmask.c, built to hostmask.so, stripped). It is embedded so a
// deployment that ships only the sandboxd binary still has the shim; it is
// written to a host directory the workspace cannot write as an opaquely-named
// file and bind-mounted read-only into every sandbox. See mask.go and
// hostmask.c. The build strips engine strings and the compiler .comment line so
// a tenant running `strings` on it learns nothing.
//
//go:embed hostmask.so
var HostMaskShim []byte
