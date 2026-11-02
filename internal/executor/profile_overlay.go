package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sandboxd/internal/profile"
)

// This file materializes a workspace PROFILE into a curated filesystem tree.
//
// It replaces the previous denylist model (copy almost all of /usr/bin and
// /usr/sbin, drop only the engine/platform names). A profile is an explicit
// ALLOWLIST: only the commands it names, plus engine-provided base commands,
// exist in the workspace. The whole /usr is replaced by the materialized tree,
// so un-allowlisted host material (/usr/include, /usr/libexec, /usr/share/perl,
// apt, the gcc chain, ...) cannot leak through an un-overlaid directory.
//
// A tool is never just its binary: the profile's `paths` entries attach the
// data/library trees a tool needs (python's stdlib, git's git-core, perl's
// share/perl). Every tool is verified by EXECUTING it in a real workspace
// (scripts/identity-check.sh), never by checking a file exists.
//
// The engine's existing engine/platform filter still applies on top: a
// profile entry whose NAME or CONTENT names the engine or platform is refused
// (fail loud) rather than silently shipped.

// baseLibraryDirs are the shared-library and loader directories the engine
// ALWAYS reproduces, regardless of profile, because every dynamically-linked
// binary needs them. They are library trees (not command sets), so including
// them does not widen the allowlist of TOOLS a workspace can run.
var baseLibraryDirs = []string{
	"/usr/lib/x86_64-linux-gnu",
	"/usr/lib64",
	"/usr/lib/locale",
	// /usr/lib/ssl holds the OpenSSL default trust paths (cert.pem and certs
	// both point into /etc/ssl/certs). Python, curl and every TLS client resolve
	// their default CA bundle through it; without it a workspace cannot verify
	// any TLS certificate even though the curated /etc/ssl/certs is mounted
	// (measured: `urllib.request.urlopen("https://pypi.org/...")` failed with
	// "unable to get local issuer certificate" until /usr/lib/ssl was included).
	"/usr/lib/ssl",
}

// profileOverlay is the materialized result for one profile.
type profileOverlay struct {
	// UsrRoot is the host directory to bind at /usr (contains bin, lib, lib64,
	// share, ...). It is also the source for /bin, /sbin, /lib and /lib64.
	UsrRoot string
	// ExtraMounts maps a jail path (e.g. /etc/ssh/ssh_config) to the host path
	// that must be mounted there, for the profile's non-/usr paths.
	ExtraMounts map[string]string
	// Commands is the sorted set of command names present in UsrRoot/bin. It is
	// the reviewable allowlist used for profile-aware command resolution.
	Commands map[string]bool
	// Warnings records non-fatal adjustments (a host command that does not
	// exist and was therefore skipped). Surfaced for truthful diagnostics.
	Warnings []string
}

// resolveInRoot resolves a bare command name against sourceRoot. When root is
// "/" it uses the host PATH (production). When root is a fixture it looks in
// <root>/usr/bin, <root>/usr/sbin, <root>/bin, following one symlink level to
// the real file -- enough for a unit-test fixture tree.
func resolveInRoot(root, name string) (string, string, error) {
	if root == "" || root == "/" {
		p, err := resolveCommand(name)
		return p, p, err
	}
	for _, d := range []string{"/usr/bin", "/usr/sbin", "/bin", "/sbin"} {
		p := filepath.Join(root, d, name)
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, rerr := os.Readlink(p)
			if rerr == nil {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(p), target)
				}
				if _, serr := os.Stat(target); serr == nil {
					return p, target, nil
				}
			}
			return p, p, nil
		}
		return p, p, nil
	}
	return "", "", ErrCommandNotFound
}

// materializeProfile builds the curated tree for a profile under dir, reading
// host material from sourceRoot ("/" in production).
func materializeProfile(dir, sourceRoot string, p *profile.Profile) (profileOverlay, error) {
	out := profileOverlay{ExtraMounts: map[string]string{}, Commands: map[string]bool{}}
	// Start from a clean tree: a service restart reuses the same tmp dir, and a
	// stale tree would collide on a symlink or keep an engine entry the profile
	// no longer names. The profile tree is fully derived from the profile file,
	// so removing it is safe.
	if err := os.RemoveAll(dir); err != nil {
		return out, err
	}
	usr := filepath.Join(dir, "usr")
	binDst := filepath.Join(usr, "bin")
	// Create the conventional subtrees up front: the template binds /sbin,
	// /lib and /lib64 to subtrees of this tree, and a mandatory bind of a
	// missing source directory aborts the run. An empty directory is the
	// correct state for a profile that names no such tool.
	if err := os.MkdirAll(binDst, 0o755); err != nil {
		return out, err
	}
	for _, sub := range []string{"sbin", "lib", "lib64", "share"} {
		if err := os.MkdirAll(filepath.Join(usr, sub), 0o755); err != nil {
			return out, err
		}
	}

	names := map[string]bool{}
	for _, c := range profile.BaseCommands {
		names[c] = true
	}
	for _, c := range p.Commands {
		names[c] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	for _, name := range sorted {
		if containsDeniedString(name) {
			return out, fmt.Errorf("profile %q command %q names the engine or platform; refusing", p.Name, name)
		}
		if err := materializeCommand(sourceRoot, binDst, name); err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("command %q not present on host: %v", name, err))
			continue
		}
		out.Commands[name] = true
	}

	for alias, target := range p.Aliases {
		if containsDeniedString(alias) {
			return out, fmt.Errorf("profile %q alias %q names the engine or platform; refusing", p.Name, alias)
		}
		if !out.Commands[target] {
			out.Warnings = append(out.Warnings, fmt.Sprintf("alias %q targets %q which is not present; skipped", alias, target))
			continue
		}
		aliasPath := filepath.Join(binDst, alias)
		_ = os.RemoveAll(aliasPath)
		if err := os.Symlink(target, aliasPath); err != nil {
			return out, fmt.Errorf("profile %q alias %q: %w", p.Name, alias, err)
		}
		out.Commands[alias] = true
	}

	// Base shared libraries and loaders (library trees, no command names).
	for _, lib := range baseLibraryDirs {
		src := filepath.Join(sourceRoot, lib)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(usr, strings.TrimPrefix(lib, "/usr/"))
		if err := buildToolDir(src, dst); err != nil {
			return out, fmt.Errorf("materialize base library %s: %w", lib, err)
		}
	}

	// Profile paths: files, directories and whole trees a tool needs.
	for _, pth := range p.Paths {
		if containsDeniedString(pth) {
			return out, fmt.Errorf("profile %q path %q names the engine or platform; refusing", p.Name, pth)
		}
		src := filepath.Join(sourceRoot, pth)
		fi, err := os.Lstat(src)
		if err != nil {
			out.Warnings = append(out.Warnings, fmt.Sprintf("path %q not present on host: %v", pth, err))
			continue
		}
		if strings.HasPrefix(pth, "/usr/") {
			dst := filepath.Join(usr, strings.TrimPrefix(pth, "/usr/"))
			if err := copyEntry(src, dst, fi); err != nil {
				return out, fmt.Errorf("materialize %s: %w", pth, err)
			}
			continue
		}
		dst := filepath.Join(dir, "mounts", strings.TrimPrefix(pth, "/"))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return out, err
		}
		if err := copyEntry(src, dst, fi); err != nil {
			return out, fmt.Errorf("materialize %s: %w", pth, err)
		}
		out.ExtraMounts[pth] = dst
	}

	out.UsrRoot = usr
	return out, nil
}

// materializeCommand places one command in dstBin, reading from sourceRoot.
func materializeCommand(sourceRoot, dstBin, name string) error {
	link, resolved, err := resolveInRoot(sourceRoot, name)
	if err != nil {
		return err
	}
	if containsDeniedString(filepath.Base(link)) {
		return fmt.Errorf("command %q names the engine or platform", name)
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	dst := filepath.Join(dstBin, name)
	// A same-directory target (the resolved file lives in /usr/bin, e.g.
	// python3 -> python3.12) is copied as real bytes so the name works
	// standalone. A link elsewhere (npm -> /usr/lib/node_modules/npm/...) keeps
	// its symlink so the tool's relative paths resolve; its target tree is
	// attached via the profile's paths.
	if filepath.Dir(link) == filepath.Dir(resolved) || filepath.Dir(resolved) == "/usr/bin" || filepath.Dir(resolved) == "/usr/sbin" {
		if fi.Mode().IsRegular() && fi.Size() <= maxToolContentScan {
			if b, rerr := os.ReadFile(resolved); rerr == nil && containsDeniedBytes(b) {
				return fmt.Errorf("command %q contains an engine/platform string", name)
			}
		}
		return cloneOrCopyFile(resolved, dst, fi.Mode().Perm())
	}
	if link != resolved {
		if target, lerr := os.Readlink(link); lerr == nil {
			return os.Symlink(target, dst)
		}
	}
	return cloneOrCopyFile(resolved, dst, fi.Mode().Perm())
}

// copyEntry reproduces a file, directory or symlink tree at dst, applying the
// engine/platform filter to names and contents. It is IDEMPOTENT: base library
// dirs and profile paths can overlap (e.g. /usr/lib/x86_64-linux-gnu/perl is
// both a base-library subtree and an explicitly-attached perl tree), so an
// existing destination entry is replaced rather than erroring on "file exists".
func copyEntry(src, dst string, fi os.FileInfo) error {
	if containsDeniedString(filepath.Base(src)) {
		return nil
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if containsDeniedString(filepath.Base(target)) {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.RemoveAll(dst)
		return os.Symlink(target, dst)
	case fi.IsDir():
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			sp := filepath.Join(src, e.Name())
			sfi, lerr := os.Lstat(sp)
			if lerr != nil {
				continue
			}
			if err := copyEntry(sp, filepath.Join(dst, e.Name()), sfi); err != nil {
				return err
			}
		}
		return nil
	default:
		if fi.Mode().IsRegular() && fi.Size() <= maxToolContentScan {
			if b, rerr := os.ReadFile(src); rerr == nil && containsDeniedBytes(b) {
				return nil
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.RemoveAll(dst)
		return cloneOrCopyFile(src, dst, fi.Mode().Perm())
	}
}
