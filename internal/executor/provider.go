package executor

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// This file implements the "provider identity" part of host masking: hiding the
// hosting platform's OWN material from a workspace. The sandbox bind-mounts the
// host's /usr so a workload has a normal toolchain, but that also exposes
//
//   - /usr/local/bin/{zcp,zsc,yq} and the Go toolchain: the platform's control
//     binaries embed the vendor's name ("ZEROPS_Y", "github.com/zeropsio");
//   - /usr/local/sbin/includes/basic_inc and the vendor CA sources under
//     /usr/local/share/ca-certificates, which name the vendor ("Zerops",
//     "vshosting.cz");
//   - /etc/ssl/certs, whose provider-issued certificates name the vendor in
//     their subject/issuer.
//
// A workspace may legitimately use nothing under /usr/local (python3, node,
// bash, perl and git all live under /usr). The service therefore overlays
// /usr/local with a curated directory that contains no host material, and
// overlays /etc/ssl/certs with a copy that omits the provider's certificates
// and a bundle filtered of them. Both overlays are read-only bind mounts, so a
// workspace cannot see or restore the host's copies.
//
// The curated directory is operator-providable (identity.tools_dir) so an image
// can ship exactly the tools a workspace may use; when unset the service
// materializes an empty one, which is the safe default. The TLS directory is
// always materialized from the host's own trust store minus the provider
// certificates, so normal TLS keeps working.

// providerStrings are substrings that identify the hosting platform's material.
// A certificate whose DER/PEM text or a filename contains one of these is not
// exposed to a workspace. They are matched case-insensitively.
var providerStrings = []string{"zerops", "vshosting", "zabbly"}

// engineStrings are substrings that identify the sandbox engine or its masking
// mechanism. They must not appear anywhere in a curated tool directory: the
// engine's own runner (`nsjail`), the masking shim (`hostmask`, `sandboxd`) and
// the engine-named environment would otherwise be visible to a tenant. They are
// distinct from providerStrings (the platform's names) and are matched
// case-insensitively.
var engineStrings = []string{"sandboxd", "hostmask", "nsjail", "SANDBOXD"}

// engineToolNames are the executables a workspace must never see. They either
// name the engine (`nsjail`, `slirp4netns`) or grant the ability to manipulate
// sandbox primitives (`runcon`, `setpriv`, `unshare`, `nsenter`, `chroot`). They
// are denied by NAME in the curated tool directories, so a host binary that
// merely embeds one of these words is not mistaken for the tool itself. Names
// are compared lower-cased.
var engineToolNames = map[string]bool{
	"nsjail":      true,
	"slirp4netns": true,
	"runcon":      true,
	"setpriv":     true,
	"unshare":     true,
	"nsenter":     true,
	"chroot":      true,
}

// containsProviderString reports whether s names the provider.
func containsProviderString(s string) bool {
	l := strings.ToLower(s)
	for _, p := range providerStrings {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
}

// containsEngineString reports whether s names the engine or its masking
// mechanism.
func containsEngineString(s string) bool {
	l := strings.ToLower(s)
	for _, e := range engineStrings {
		if strings.Contains(l, strings.ToLower(e)) {
			return true
		}
	}
	return false
}

// containsDeniedString reports whether s names the provider OR the engine.
func containsDeniedString(s string) bool {
	return containsProviderString(s) || containsEngineString(s)
}

// containsDeniedBytes reports whether b contains a provider or engine name. It
// lower-cases the input once and searches for each token, so a binary that
// embeds a vendor or engine string anywhere is caught (a `strings`-style audit,
// not a presence check).
func containsDeniedBytes(b []byte) bool {
	low := strings.ToLower(string(b))
	for _, p := range providerStrings {
		if strings.Contains(low, p) {
			return true
		}
	}
	for _, e := range engineStrings {
		if strings.Contains(low, strings.ToLower(e)) {
			return true
		}
	}
	return false
}

// maxToolContentScan bounds how large a curated-directory file is read for the
// content audit. Tool binaries are small; a larger file is a data blob that does
// not define a command, and is skipped so curating a directory stays fast.
const maxToolContentScan = 64 << 20

// buildToolDir materializes a curated copy of a host tool directory (e.g.
// /usr/bin) under dst. An entry is omitted when:
//
//   - its NAME is an engineToolName or names the provider/engine (so `nsjail`,
//     `slirp4netns`, `runcon`, `setpriv`, `unshare`, `nsenter`, `chroot` and any
//     engine/platform-named file are gone);
//   - it is a regular file whose CONTENT contains a provider or engine string
//     (so a binary embedding `zerops`, `hostmask`, `sandboxd`, … cannot slip
//     through under a bland name);
//   - it is a symlink whose target does not resolve (a dangling link whose
//     name would be the only surviving trace).
//
// Every other entry is reproduced so a workspace keeps its full legitimate
// toolchain. The copy is a reflink/clone of the host file where the filesystem
// supports it (measured on the target host) and a byte copy otherwise, so this
// stays cheap and the tenant can never write the source.
func buildToolDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if engineToolNames[strings.ToLower(name)] || containsDeniedString(name) {
			continue
		}
		sp := filepath.Join(src, name)
		dp := filepath.Join(dst, name)
		fi, lerr := os.Lstat(sp)
		if lerr != nil {
			continue
		}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, rerr := os.Readlink(sp)
			if rerr != nil {
				continue
			}
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(src, resolved)
			}
			if _, serr := os.Stat(resolved); serr != nil {
				continue // dangling: its name would be the only trace
			}
			if engineToolNames[strings.ToLower(filepath.Base(resolved))] ||
				containsDeniedString(filepath.Base(resolved)) {
				continue
			}
			if err := os.Symlink(target, dp); err != nil {
				return err
			}
		case fi.Mode().IsRegular():
			if fi.Size() <= maxToolContentScan {
				if b, rerr := os.ReadFile(sp); rerr == nil && containsDeniedBytes(b) {
					continue
				}
			}
			if err := cloneOrCopyFile(sp, dp, fi.Mode().Perm()); err != nil {
				return err
			}
		default:
			// Directories and special files are not tool entries; skip.
			continue
		}
	}
	return nil
}

// cloneOrCopyFile reproduces a file into a curated directory. It first tries an
// ioctl FICLONE (a reflink, supported by the target host's filesystem), then a
// hard link (same result for an immutable, read-only-mounted tree, and cheap),
// and finally a streaming byte copy. The destination is never writable by a
// tenant: the curated tree is bind-mounted read-only.
func cloneOrCopyFile(src, dst string, perm os.FileMode) error {
	if err := ficlone(src, dst); err == nil {
		return os.Chmod(dst, perm)
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	if err := copyFileBytes(src, dst, perm); err != nil {
		return err
	}
	return nil
}

// ficlone performs a filesystem reflink of src to dst. It is a no-op error on a
// filesystem that does not support it, and the caller falls back.
func ficlone(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd())); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// copyFileBytes streams a file, preserving its mode.
func copyFileBytes(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// buildFilteredPasswd writes a copy of /etc/passwd with every entry whose name
// or comment names the provider or the engine removed. The host account file
// carries the operator's own account (`zerops`), which would name the platform
// to any `getent passwd`/`id`. The accounts a workspace legitimately needs
// (root, nobody, its own service users) are reproduced unchanged.
func buildFilteredPasswd(src, dst string) error {
	return buildFilteredAccountFile(src, dst, containsDeniedString)
}

// buildFilteredGroup writes a copy of /etc/group with provider/engine-named
// groups removed, using the same rule as buildFilteredPasswd.
func buildFilteredGroup(src, dst string) error {
	return buildFilteredAccountFile(src, dst, containsDeniedString)
}

// buildFilteredAccountFile copies a colon-separated account file, dropping a
// line when the "denied" predicate matches its name (field 1) or its comment
// (field 5, when present). A line that cannot be parsed is dropped too, because
// an unparsed entry is exactly the kind that would smuggle a name through.
func buildFilteredAccountFile(src, dst string, denied func(string) bool) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	var out []byte
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			out = append(out, '\n')
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		name := fields[0]
		comment := ""
		if len(fields) > 4 {
			comment = fields[4]
		}
		if denied(name) || denied(comment) {
			continue
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return os.WriteFile(dst, out, 0o644)
}

// providerNamesProviderSource reports whether a /etc/ssl/certs symlink target
// is provider material, i.e. it lives under /usr/local/share/ca-certificates
// (where the platform installs its own CAs) or its text names the provider.
func providerSourceFromPath(target string) bool {
	return strings.HasPrefix(target, "/usr/local/")
}

// buildEmptyToolsDir materializes an empty curated /usr/local overlay with the
// standard bin/lib subdirectories. It is the safe default when no operator
// tools directory is configured: the workspace keeps the whole of /usr/bin and
// sees no host /usr/local material.
func buildEmptyToolsDir(dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	for _, sub := range []string{"bin", "lib", "sbin", "share"} {
		if err := os.MkdirAll(filepath.Join(dst, sub), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// buildCertsDir materializes the curated /etc/ssl/certs overlay from the host's
// own trust store. A certificate whose decoded DER or whose source path names
// the provider is omitted; every other entry is reproduced as a relative
// symlink pointing back into the host trust store (which the workspace cannot
// reach directly, but the kernel resolves for it). A filtered concatenated
// bundle is written at ca-certificates.crt, so a TLS client that loads the
// standard bundle keeps working. The host's own /etc/ssl/certs is never
// modified.
func buildCertsDir(dst, src string) error {
	const bundle = "ca-certificates.crt"
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == bundle {
			continue // written filtered below
		}
		if containsProviderString(name) {
			continue
		}
		p := filepath.Join(src, name)
		// Read the resolved certificate (symlink or regular file). An entry
		// whose content names the provider is omitted.
		raw, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		if certNamesProvider(raw) {
			continue
		}
		// Reproduce as a symlink to the host's resolved file, unless that
		// resolved source is the vendor's own ca-certificates directory (whose
		// PATH names the provider); in that case copy the bytes so the vendor
		// path never appears.
		resolved, rerr := filepath.EvalSymlinks(p)
		if rerr != nil || providerSourceFromPath(resolved) {
			if err := os.WriteFile(filepath.Join(dst, name), raw, 0o644); err != nil {
				return err
			}
			continue
		}
		if err := os.Symlink(resolved, filepath.Join(dst, name)); err != nil {
			return err
		}
	}
	// Filtered bundle: keep every PEM block whose certificate does not name the
	// provider.
	raw, err := os.ReadFile(filepath.Join(src, bundle))
	if err != nil {
		// No host bundle: leave the overlay without one rather than fail; the
		// curated dir still omits the provider certificates.
		return nil
	}
	if err := os.WriteFile(filepath.Join(dst, bundle), filterCABundle(raw), 0o644); err != nil {
		return err
	}
	return nil
}

// certNamesProvider reports whether a PEM certificate names the provider in its
// decoded DER. The names live in the DER-encoded subject/issuer, so the search
// must be over the DECODED bytes, not the base64 PEM text.
func certNamesProvider(pemBytes []byte) bool {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return containsProviderString(string(pemBytes))
	}
	return containsProviderString(string(block.Bytes))
}

// filterCABundle drops every PEM certificate block whose decoded certificate
// names the provider. The input is the concatenated /etc/ssl/certs bundle; the
// output keeps the same concatenated form.
func filterCABundle(raw []byte) []byte {
	const begin = "-----BEGIN CERTIFICATE-----"
	const end = "-----END CERTIFICATE-----"
	var out []byte
	rest := raw
	for {
		i := bytes.Index(rest, []byte(begin))
		if i < 0 {
			out = append(out, rest...)
			break
		}
		out = append(out, rest[:i]...)
		rest = rest[i:]
		j := bytes.Index(rest, []byte(end))
		if j < 0 {
			out = append(out, rest...)
			break
		}
		block := rest[:j+len(end)]
		rest = rest[j+len(end):]
		if !certNamesProvider(block) {
			out = append(out, block...)
			out = append(out, '\n')
		}
	}
	return out
}

// materializeProviderOverlays decides the curated /usr/local source and builds
// the curated /etc/ssl/certs tree under dir. When the operator configured a
// tools directory it is used directly as the mount source (so a symlink inside
// it resolves against itself, and the curated set is exactly what the operator
// chose); otherwise an empty tree is materialized. A failure to build the empty
// tree is returned as an error because its mount would otherwise expose the host
// directory; a failure to read the host trust store only leaves certsDir empty
// (the /etc/ssl/certs mount is then omitted, a closed outcome), so it is not an
// error.
//
// It also builds the curated /usr/bin and /usr/sbin trees (into binDir/sbinDir).
// Those are NOT optional: the host's /usr is mounted so a workload has a normal
// toolchain, and it carries the engine's own runner (`nsjail`), the sandbox
// helpers (`slirp4netns`, `unshare`, `nsenter`, `setpriv`, `runcon`, `chroot`),
// and any engine/platform-named file. The curated copies drop exactly those
// entries and reproduce every other tool, so a workspace keeps its toolchain
// with no engine name reachable. Their build failure is returned as an error,
// because a missing curated tree would leave the host's own exposed.
// providerOverlays groups the curated host-path sources materialized for a
// sandbox's read-only overlays. Empty fields are simply not mounted.
type providerOverlays struct {
	ToolsDir string // curated /usr/local
	CertsDir string // curated /etc/ssl/certs (empty when unreadable)
	BinDir   string // curated /usr/bin
	SbinDir  string // curated /usr/sbin
	Passwd   string // filtered /etc/passwd
	Group    string // filtered /etc/group
}

func materializeProviderOverlays(dir, toolsSrc, binSrc, sbinSrc string) (providerOverlays, error) {
	var o providerOverlays
	if toolsSrc != "" {
		if fi, serr := os.Stat(toolsSrc); serr == nil && fi.IsDir() {
			o.ToolsDir = toolsSrc
		} else {
			return o, fmt.Errorf("identity.tools_dir %q is not a directory", toolsSrc)
		}
	} else {
		o.ToolsDir = filepath.Join(dir, "usr-local")
		if err := buildEmptyToolsDir(o.ToolsDir); err != nil {
			return o, fmt.Errorf("build curated /usr/local: %w", err)
		}
	}
	if binSrc == "" {
		binSrc = "/usr/bin"
	}
	if sbinSrc == "" {
		sbinSrc = "/usr/sbin"
	}
	o.BinDir = filepath.Join(dir, "usr-bin")
	if err := buildToolDir(binSrc, o.BinDir); err != nil {
		return o, fmt.Errorf("build curated /usr/bin: %w", err)
	}
	o.SbinDir = filepath.Join(dir, "usr-sbin")
	if err := buildToolDir(sbinSrc, o.SbinDir); err != nil {
		return o, fmt.Errorf("build curated /usr/sbin: %w", err)
	}
	certsDir := filepath.Join(dir, "ssl-certs")
	if cerr := buildCertsDir(certsDir, "/etc/ssl/certs"); cerr != nil {
		certsDir = ""
	}
	o.CertsDir = certsDir
	// The host account files carry the operator's own account (`zerops`),
	// which names the platform to any `getent passwd`/`id`. Reproduce them with
	// every provider/engine-named entry dropped. A failure here is a hard error
	// like the tool trees: mounting the host's own file would leak the account.
	o.Passwd = filepath.Join(dir, "passwd")
	if err := buildFilteredPasswd("/etc/passwd", o.Passwd); err != nil {
		return o, fmt.Errorf("build filtered /etc/passwd: %w", err)
	}
	o.Group = filepath.Join(dir, "group")
	if err := buildFilteredGroup("/etc/group", o.Group); err != nil {
		return o, fmt.Errorf("build filtered /etc/group: %w", err)
	}
	return o, nil
}
