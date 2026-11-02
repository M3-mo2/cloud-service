package executor

import (
	"bytes"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
func materializeProviderOverlays(dir, toolsSrc string) (toolsDir, certsDir string, err error) {
	if toolsSrc != "" {
		if fi, serr := os.Stat(toolsSrc); serr == nil && fi.IsDir() {
			toolsDir = toolsSrc
		} else {
			return "", "", fmt.Errorf("identity.tools_dir %q is not a directory", toolsSrc)
		}
	} else {
		toolsDir = filepath.Join(dir, "usr-local")
		if err = buildEmptyToolsDir(toolsDir); err != nil {
			return "", "", fmt.Errorf("build curated /usr/local: %w", err)
		}
	}
	certsDir = filepath.Join(dir, "ssl-certs")
	if cerr := buildCertsDir(certsDir, "/etc/ssl/certs"); cerr != nil {
		certsDir = ""
	}
	return toolsDir, certsDir, nil
}
