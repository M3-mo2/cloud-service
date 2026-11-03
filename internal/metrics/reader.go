package metrics

import (
	"os"
	"path/filepath"
	"strings"

	"sandboxd/internal/workspace"
)

// fileReader is the production Reader: it reads cgroup v2 files directly and
// statfs for disk. It never shells out (a `sudo -n cat` is ~9 ms versus ~23 µs
// for a direct read, a 400x penalty that alone would break the cheap
// requirement), never walks a directory tree, and never reads
// /proc/<pid>/environ (~9 ms per call).
//
// The service runs as root, so it reads a NSJAIL.* cgroup directory directly.
type fileReader struct{}

// NewFileReader returns the production Reader.
func NewFileReader() Reader { return fileReader{} }

// MemoryCurrent reads a cgroup's memory.current.
func (fileReader) MemoryCurrent(dir string) (int64, error) {
	return readIntFile(filepath.Join(dir, "memory.current"))
}

// MemoryMax reads a cgroup's memory.max. A literal "max" (no ceiling) yields 0.
func (fileReader) MemoryMax(dir string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, "memory.max"))
	if err != nil {
		return 0, err
	}
	if s := strings.TrimSpace(string(b)); s == "max" {
		return 0, nil
	}
	return parseBytes(b)
}

// CPUUsageUsec reads a cgroup's cpu.stat usage_usec.
func (fileReader) CPUUsageUsec(dir string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return 0, err
	}
	return parseStatUsec(b)
}

// PidsCurrent reads a cgroup's pids.current.
func (fileReader) PidsCurrent(dir string) (int64, error) {
	return readIntFile(filepath.Join(dir, "pids.current"))
}

// DiskUsed returns the used bytes of a live mounted workspace filesystem via
// one statfs, and ok=false when the path is not a live mount.
func (fileReader) DiskUsed(root string) (int64, bool) {
	return workspace.DiskUsed(root)
}

func readIntFile(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return parseBytes(b)
}
