// Package marker manages single-use marker files keyed by a git write-tree
// hash. A marker is a zero-byte file at <dir>/<hash>; consuming it deletes
// the file atomically and reports that the corresponding staged tree has
// already been reviewed.
package marker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Consume returns true if a marker file for hash exists in dir and was
// successfully removed. It returns false if no marker existed, with err nil.
// It returns false with a non-nil err if dir exists but is unsafe (wrong
// owner or group/world-accessible perms) or if any I/O error other than
// "not found" occurs.
//
// A missing dir is treated as "no marker present" (false, nil) so callers
// can defer creating the dir until they decide to write one.
func Consume(dir, hash string) (bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("marker path %s is not a directory", dir)
	}
	if err := checkDirSafety(dir, info); err != nil {
		return false, err
	}

	markerPath := filepath.Join(dir, hash)
	if err := os.Remove(markerPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// EnsureDir creates dir with mode 0o700 if it does not already exist, and
// enforces mode 0o700 on the dir regardless of how it was created. Returns
// any I/O error encountered.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

// checkDirSafety verifies that dir is owned by the current user and that
// no group/world bits are set on its mode.
func checkDirSafety(dir string, info os.FileInfo) error {
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("marker dir %s has unsafe mode %#o (group/world bits set)", dir, mode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// On platforms without Stat_t (unlikely on macOS/Linux), we accept the
		// dir if the mode check passed. The hook is designed for Unix-likes.
		return nil
	}
	if int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("marker dir %s not owned by current uid (%d != %d)", dir, stat.Uid, os.Geteuid())
	}
	return nil
}
