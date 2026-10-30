package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

// ErrUnsafePath is returned when a user-supplied path escapes the workspace
// root or resolves through a symlink that leaves the workspace.
type pathError struct{ msg string }

func (e *pathError) Error() string { return e.msg }

var (
	errTraversal    = &pathError{"path escapes workspace root"}
	errSymlinkEsc   = &pathError{"symlink points outside workspace root"}
	errNotDirectory = &pathError{"path is not a directory"}
)

// IsUnsafe reports whether err indicates an unsafe path.
func IsUnsafe(err error) bool {
	_, ok := err.(*pathError)
	return ok
}

// cleanRelative validates and normalizes a user path to a cleaned, slash-based
// relative path that cannot escape the workspace root.
//
// Leading slashes are allowed and stripped. Any `..` component that would
// climb above the root is rejected (errTraversal); interior `..` that stays
// within the root is resolved normally.
func cleanRelative(userPath string) (string, error) {
	p := strings.ReplaceAll(userPath, "\\", "/")
	// Treat paths as rooted at the workspace; strip the leading slash(es).
	for strings.HasPrefix(p, "/") {
		p = p[1:]
	}
	if p == "" {
		return ".", nil
	}
	stack := make([]string, 0, 4)
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return "", errTraversal
			}
			stack = stack[:len(stack)-1]
		default:
			stack = append(stack, part)
		}
	}
	if len(stack) == 0 {
		return ".", nil
	}
	return strings.Join(stack, "/"), nil
}

// within reports whether target is inside (or equal to) root.
func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// secureJoin resolves cleanRel against root, walking each component with
// Lstat and rejecting any symlink whose target escapes root. The returned
// path is absolute and guaranteed to be within root.
func secureJoin(root, cleanRel string) (string, error) {
	if cleanRel == "." {
		return root, nil
	}
	parts := strings.Split(cleanRel, "/")
	cur := root
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// Remaining components do not exist; no symlink can exist
				// beneath a non-existent component.
				rest := filepath.Join(parts[i+1:]...)
				out := filepath.Join(cur, rest)
				if !within(root, out) {
					return "", errTraversal
				}
				return out, nil
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			if !within(root, resolved) {
				return "", errSymlinkEsc
			}
			cur = resolved
		}
	}
	if !within(root, cur) {
		return "", errTraversal
	}
	return cur, nil
}
