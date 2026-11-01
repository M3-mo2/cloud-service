package service

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sandboxd/internal/executor"
	"sandboxd/internal/model"
)

// Sandbox mount points for the two package tiers. These are the paths passed
// to the interpreter, not host paths.
const (
	// SharedPackagesMount is where the shared read-only tier is mounted.
	SharedPackagesMount = "/opt/python/shared"
	// PrivatePackagesMount is where the workspace's writable tier is mounted.
	PrivatePackagesMount = "/opt/python/private"
)

// maxRequirementLen bounds a single requirement specifier. A 10k-character
// requirement is rejected outright.
const maxRequirementLen = 200

// maxListedPackages bounds how many private-tier distributions GET /packages
// enumerates. Larger directories are reported with truncated=true.
const maxListedPackages = 1000

// PrivatePackage is one distribution installed in a workspace's private tier.
type PrivatePackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// PackageTier describes the shared tier's availability and the workspace's
// private tier path for API rendering.
type PackageTier struct {
	SharedEnabled   bool
	SharedAvailable bool
	SharedPath      string
	PrivatePath     string
}

// ErrPackageNotFound signals that a requested package is not installed.
var ErrPackageNotFound = model.New(404, model.CodeNotFound, "package not found")

// ValidateRequirement rejects anything that is not a plausible PEP 508-ish
// requirement specifier. The value is handed to `pip` as a single argv element
// (never through a shell), so shell metacharacters cannot inject a command
// line, but a strict allowlist is enforced anyway as defense in depth:
//
//   - empty or longer than maxRequirementLen is rejected;
//   - the first character must be alphanumeric (rejects `-e`, `--upgrade`,
//     `../../etc/passwd`);
//   - only [A-Za-z0-9._+~*=<>,!\[\]-] are allowed (rejects `;`, spaces,
//     quotes, `$`, backticks, `/`, `\`, `|`, `&`, parentheses, ...);
//   - extras brackets must be balanced.
func ValidateRequirement(req string) error {
	invalid := func(reason string) error {
		return model.Wrap(422, model.CodeUnprocessable, "invalid requirement specifier: "+reason,
			map[string]string{"requirement": truncateForDetail(req)})
	}
	if req == "" {
		return invalid("empty")
	}
	if len(req) > maxRequirementLen {
		return invalid("too long")
	}
	if !isAlnum(req[0]) {
		return invalid("must start with a letter or digit")
	}
	depth := 0
	for i := 0; i < len(req); i++ {
		c := req[i]
		switch {
		case isAlnum(c):
		case c == '.' || c == '_' || c == '-' || c == '+' || c == '~' ||
			c == '*' || c == '=' || c == '<' || c == '>' || c == '!':
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth < 0 {
				return invalid("unbalanced brackets")
			}
		case c == ',':
		default:
			return invalid(fmt.Sprintf("disallowed character %q", string(c)))
		}
	}
	if depth != 0 {
		return invalid("unbalanced brackets")
	}
	return nil
}

func truncateForDetail(s string) string {
	const max = 64
	if len(s) > max {
		return s[:max]
	}
	return s
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// PackageTierFor resolves the currently effective tier information for a
// workspace. shared availability is probed fresh so an operator can drop the
// directory in without a restart; a missing, empty or unreadable shared dir
// simply reports available=false and runs continue with the private tier.
func (s *Service) PackageTierFor(w *model.Workspace) PackageTier {
	t := PackageTier{PrivatePath: PrivatePackagesMount}
	if !s.cfg.Packages.SharedEnabled || s.cfg.Packages.SharedDir == "" {
		return t
	}
	t.SharedEnabled = true
	if dirAvailable(s.cfg.Packages.SharedDir) {
		t.SharedAvailable = true
		t.SharedPath = SharedPackagesMount
	}
	return t
}

// packageDirs returns the host-side paths the executor should bind-mount:
// the shared dir (empty unless available/enabled) and the workspace's private
// dir. It never returns a private path that escapes the workspace root.
func (s *Service) packageDirs(w *model.Workspace) (sharedHost, privateHost string, err error) {
	if s.cfg.Packages.SharedEnabled && dirAvailable(s.cfg.Packages.SharedDir) {
		sharedHost = s.cfg.Packages.SharedDir
	}
	privateHost = s.store.WorkspacePackagesDir(w.ID)
	if err := s.ensurePrivateDir(w, privateHost); err != nil {
		return "", "", err
	}
	return sharedHost, privateHost, nil
}

// ensurePrivateDir creates the workspace's private package directory and, when
// the orchestrator is privileged, hands it to the sandbox identity so the
// workload can write it. The directory lives inside the workspace data dir, so
// the workspace path-safety helpers still bound it.
func (s *Service) ensurePrivateDir(w *model.Workspace, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if id, ok := s.runner.(interface{ Identity() executor.Identity }); ok {
		if err := id.Identity().ChownToSandbox(dir); err != nil {
			return err
		}
	}
	return nil
}

// dirAvailable reports whether dir exists and is a readable directory.
func dirAvailable(dir string) bool {
	if dir == "" {
		return false
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false
	}
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	_, _ = f.Readdirnames(1)
	f.Close()
	return true
}

// InstallResult is the outcome of a pip install in a workspace.
type InstallResult struct {
	ExitCode   int
	Output     string
	TimedOut   bool
	Truncated  bool
	DurationMS int64
}

// ListPrivatePackages enumerates the distributions in a workspace's private
// tier by reading *.dist-info / *.egg-info metadata. Enumeration is bounded by
// maxListedPackages; truncated reports that the bound was reached.
func (s *Service) ListPrivatePackages(w *model.Workspace) (pkgs []PrivatePackage, truncated bool, err error) {
	if err := s.ensureActive(w); err != nil {
		return nil, false, err
	}
	dir := s.store.WorkspacePackagesDir(w.ID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []PrivatePackage{}, false, nil
		}
		return nil, false, err
	}
	found := map[string]PrivatePackage{}
	for _, e := range entries {
		if len(found) >= maxListedPackages {
			truncated = true
			break
		}
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		var p PrivatePackage
		switch {
		case strings.HasSuffix(name, ".dist-info"):
			p = readDistInfo(filepath.Join(dir, name), strings.TrimSuffix(name, ".dist-info"))
		case strings.HasSuffix(name, ".egg-info"):
			p = readEggInfo(filepath.Join(dir, name), strings.TrimSuffix(name, ".egg-info"))
		default:
			continue
		}
		if p.Name == "" {
			continue
		}
		found[normalizeName(p.Name)] = p
	}
	pkgs = make([]PrivatePackage, 0, len(found))
	for _, p := range found {
		pkgs = append(pkgs, p)
	}
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Name < pkgs[j].Name })
	return pkgs, truncated, nil
}

// readDistInfo reads Name/Version from a *.dist-info directory's METADATA,
// falling back to the directory name (<name>-<version>) when METADATA is
// unavailable.
func readDistInfo(dir, fallback string) PrivatePackage {
	if p, ok := parseMetadataFile(filepath.Join(dir, "METADATA")); ok {
		return p
	}
	name, version := splitNameVersion(fallback)
	return PrivatePackage{Name: name, Version: version}
}

// readEggInfo reads Name/Version from a *.egg-info directory's PKG-INFO,
// falling back to the directory name.
func readEggInfo(dir, fallback string) PrivatePackage {
	if p, ok := parseMetadataFile(filepath.Join(dir, "PKG-INFO")); ok {
		return p
	}
	name, version := splitNameVersion(fallback)
	return PrivatePackage{Name: name, Version: version}
}

// parseMetadataFile reads RFC 822-style Name:/Version: headers.
func parseMetadataFile(path string) (PrivatePackage, bool) {
	f, err := os.Open(path)
	if err != nil {
		return PrivatePackage{}, false
	}
	defer f.Close()
	var p PrivatePackage
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Name:"); ok {
			p.Name = strings.TrimSpace(v)
		}
		if v, ok := strings.CutPrefix(line, "Version:"); ok {
			p.Version = strings.TrimSpace(v)
		}
		if p.Name != "" && p.Version != "" {
			break
		}
	}
	return p, p.Name != ""
}

// splitNameVersion splits a "<name>-<version>" stem on the last hyphen.
func splitNameVersion(stem string) (name, version string) {
	i := strings.LastIndex(stem, "-")
	if i <= 0 {
		return stem, ""
	}
	return stem[:i], stem[i+1:]
}

// normalizeName applies PEP 503 name normalization.
func normalizeName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	prevSep := false
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' {
			if !prevSep {
				b.WriteByte('-')
				prevSep = true
			}
			continue
		}
		prevSep = false
		b.WriteRune(r)
	}
	return b.String()
}

// InstallPackages validates the requirement specifiers and installs them into
// the workspace's private tier by running `pip install --target
// /opt/python/private` inside the workspace's own nsjail sandbox. Running pip
// in the sandbox means package setup code executes as the unprivileged sandbox
// uid, never as the orchestrator, and cannot write outside the workspace. The
// shared tier is mounted read-only, so pip cannot touch it.
//
// Installation requires network access; a workspace with allow_net=false will
// see pip fail to reach an index (captured in Output). Tests use a fake runner
// and never require network.
func (s *Service) InstallPackages(w *model.Workspace, reqs []string) (InstallResult, error) {
	if err := s.ensureActive(w); err != nil {
		return InstallResult{}, err
	}
	if len(reqs) == 0 {
		return InstallResult{}, model.Wrap(422, model.CodeUnprocessable, "at least one requirement is required", nil)
	}
	for _, r := range reqs {
		if err := ValidateRequirement(r); err != nil {
			return InstallResult{}, err
		}
	}
	sharedHost, privateHost, err := s.packageDirs(w)
	if err != nil {
		return InstallResult{}, err
	}

	argv := append([]string{"python3", "-m", "pip", "install", "--no-input", "--disable-pip-version-check", "--target", PrivatePackagesMount}, reqs...)

	outCap := s.cfg.Packages.InstallOutputBytes()
	req := executor.Request{
		Workspace:          w,
		WorkspaceRoot:      s.store.WorkspaceRoot(w.ID),
		Argv:               argv,
		Cwd:                "/",
		Env:                map[string]string{"PYTHONDONTWRITEBYTECODE": "1"},
		Timeout:            s.cfg.Packages.InstallTimeout(),
		StdoutCap:          outCap,
		StderrCap:          outCap,
		SharedPackagesDir:  sharedHost,
		PrivatePackagesDir: privateHost,
	}
	start := time.Now()
	res, err := s.runner.Run(req)
	if err != nil {
		return InstallResult{}, err
	}
	out := string(res.Stdout)
	if len(res.Stderr) > 0 {
		if out != "" {
			out += "\n"
		}
		out += string(res.Stderr)
	}
	truncated := res.Truncated
	if int64(len(out)) > outCap {
		out = out[:outCap]
		truncated = true
	}
	s.touchActivity(w.ID)
	return InstallResult{
		ExitCode:   res.ExitCode,
		Output:     out,
		TimedOut:   res.TimedOut,
		Truncated:  truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}, nil
}

// RemovePackage removes a single distribution from the workspace's private
// tier. It reads the distribution's RECORD file so only files that belong to
// that distribution are deleted. It returns ErrPackageNotFound when no
// matching distribution is installed.
func (s *Service) RemovePackage(w *model.Workspace, name string) error {
	if err := s.ensureActive(w); err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return model.Wrap(400, model.CodeValidation, "name is required", nil)
	}
	dir := s.store.WorkspacePackagesDir(w.ID)
	distDir, ok := findDistribution(dir, name)
	if !ok {
		return ErrPackageNotFound
	}
	if err := removeDistribution(dir, distDir); err != nil {
		return err
	}
	s.touchActivity(w.ID)
	return nil
}

// RemoveAllPackages removes every distribution from the workspace's private
// tier and returns the number of top-level entries deleted. It is deliberately
// separate from RemovePackage so "remove all" can never be triggered by an
// empty name.
func (s *Service) RemoveAllPackages(w *model.Workspace) (int, error) {
	if err := s.ensureActive(w); err != nil {
		return 0, err
	}
	dir := s.store.WorkspacePackagesDir(w.ID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	s.touchActivity(w.ID)
	return removed, nil
}

// findDistribution returns the *.dist-info or *.egg-info directory for name.
func findDistribution(dir, name string) (string, bool) {
	want := normalizeName(name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		stem := e.Name()
		var meta string
		var fallback string
		switch {
		case strings.HasSuffix(stem, ".dist-info"):
			fallback = strings.TrimSuffix(stem, ".dist-info")
			meta = filepath.Join(dir, stem, "METADATA")
		case strings.HasSuffix(stem, ".egg-info"):
			fallback = strings.TrimSuffix(stem, ".egg-info")
			meta = filepath.Join(dir, stem, "PKG-INFO")
		default:
			continue
		}
		var got string
		if p, ok := parseMetadataFile(meta); ok {
			got = p.Name
		} else {
			got, _ = splitNameVersion(fallback)
		}
		if normalizeName(got) == want {
			return filepath.Join(dir, stem), true
		}
	}
	return "", false
}

// removeDistribution deletes the files recorded in a dist-info's RECORD (or an
// egg-info plus any top-level module/package with the same normalized name),
// then removes the metadata directory.
func removeDistribution(dir, distDir string) error {
	if strings.HasSuffix(distDir, ".dist-info") {
		record := filepath.Join(distDir, "RECORD")
		if err := removeRecorded(dir, record); err != nil {
			return err
		}
	} else {
		stem := strings.TrimSuffix(filepath.Base(distDir), ".egg-info")
		name, _ := splitNameVersion(stem)
		for _, candidate := range []string{name, strings.ReplaceAll(name, "-", "_")} {
			if candidate == "" {
				continue
			}
			_ = os.RemoveAll(filepath.Join(dir, candidate))
		}
	}
	if err := os.RemoveAll(distDir); err != nil {
		return err
	}
	return nil
}

// removeRecorded deletes each file listed in a RECORD file (CSV: path,hash,...)
// relative to dir. Missing entries are ignored.
func removeRecorded(dir, record string) error {
	f, err := os.Open(record)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	var parents []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		field := line
		if i := strings.IndexByte(line, ','); i >= 0 {
			field = line[:i]
		}
		field = strings.TrimSpace(field)
		if field == "" || field == "RECORD" {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(field))
		if !withinDir(dir, full) {
			continue
		}
		_ = os.Remove(full)
		parents = append(parents, filepath.Dir(full))
	}
	// Remove now-empty directories, deepest first.
	sort.Slice(parents, func(i, j int) bool { return len(parents[i]) > len(parents[j]) })
	for _, p := range parents {
		_ = os.Remove(p)
	}
	return nil
}

// withinDir reports whether target is inside (or equal to) dir.
func withinDir(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
