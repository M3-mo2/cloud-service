package executor

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"sandboxd/internal/config"
)

// fallback IDs used when neither config nor /etc/passwd/group resolve a sandbox
// identity. 65534 is the conventional "nobody" account and is always
// unprivileged.
const (
	fallbackUID = 65534
	fallbackGID = 65534
)

// Identity describes the UID/GID mapping used for sandboxed processes.
//
// The kernel only permits an unprivileged process to write a single-entry
// UID/GID map whose *outside* id equals its own effective id. We therefore map
// the sandbox identity *inside* the new user namespace onto an identity that is
// permitted *outside* it:
//
//   - running as root: inside sandbox(uid) <-> outside sandbox(uid), a real
//     privilege drop. The workspace is chowned to the sandbox uid so the jailed
//     process can read/write it while the root orchestrator retains access.
//   - running unprivileged: inside sandbox(uid) <-> outside orchestrator(uid).
//     The kernel permits this mapping; host files owned by the orchestrator
//     appear inside the namespace as owned by the sandbox user, so no chown is
//     needed.
type Identity struct {
	SandboxUID int
	SandboxGID int
	OutsideUID int
	OutsideGID int
	// Privileged reports whether the orchestrator runs as root (real drop).
	Privileged bool
}

// ResolveIdentity computes the sandbox identity mapping for the current process.
func ResolveIdentity(cfg config.Config) Identity {
	uid, gid := resolveSandboxIDs(cfg)
	euid, egid := os.Geteuid(), os.Getegid()

	// Map onto an id the kernel will accept as the "outside" id.
	outsideUID, outsideGID := uid, gid
	privileged := euid == 0
	if !privileged {
		outsideUID, outsideGID = euid, egid
	}
	return Identity{
		SandboxUID: uid,
		SandboxGID: gid,
		OutsideUID: outsideUID,
		OutsideGID: outsideGID,
		Privileged: privileged,
	}
}

// resolveSandboxIDs determines the sandbox uid/gid from explicit config first,
// then a user lookup, then a group lookup, finally falling back to nobody.
func resolveSandboxIDs(cfg config.Config) (int, int) {
	uid, gid := cfg.Sandbox.UID, cfg.Sandbox.GID

	if uid == 0 && cfg.Sandbox.User != "" {
		if u, err := user.Lookup(cfg.Sandbox.User); err == nil {
			if v, err := strconv.Atoi(u.Uid); err == nil {
				uid = v
			}
			if gid == 0 {
				if v, err := strconv.Atoi(u.Gid); err == nil {
					gid = v
				}
			}
		}
	}
	if gid == 0 && cfg.Sandbox.Group != "" {
		if g, err := user.LookupGroup(cfg.Sandbox.Group); err == nil {
			if v, err := strconv.Atoi(g.Gid); err == nil {
				gid = v
			}
		}
	}
	if uid == 0 {
		uid = fallbackUID
	}
	if gid == 0 {
		gid = fallbackGID
	}
	return uid, gid
}

// ChownToSandbox transfers ownership of path to the sandbox identity. It is a
// no-op unless the orchestrator is privileged, because an unprivileged
// orchestrator already maps its own files to the sandbox identity.
func (id Identity) ChownToSandbox(path string) error {
	if !id.Privileged {
		return nil
	}
	return os.Chown(path, id.SandboxUID, id.SandboxGID)
}

// chownTree recursively sets ownership of root to uid:gid. It does not follow
// symlinks. Only used when the orchestrator is privileged.
func chownTree(root string, uid, gid int) error {
	if _, err := os.Lstat(root); err != nil {
		// A missing workspace root is not fatal here; the run will fail with a
		// clearer error when nsjail mounts it.
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := os.Lchown(path, uid, gid); err != nil {
			return err
		}
		return nil
	})
}

// ensureTraversable adds the "other execute" bit to path and each ancestor up
// to the filesystem root, so a privileged orchestrator (which mounts the
// workspace as the sandbox uid) can descend to it. Every component must be
// searchable by that uid, so the walk cannot stop early. Modes are preserved;
// only o+x is added.
func ensureTraversable(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for dir := abs; ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		mode := info.Mode().Perm()
		if mode&0o001 == 0 {
			if err := os.Chmod(dir, mode|0o001); err != nil {
				return err
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return nil
}
