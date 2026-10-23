// Package materialinstall copies deployment material from a Kubernetes Secret
// volume into a directory that internal/security/securefile can read.
//
// # Why the package exists
//
// securefile refuses a path that resolves through a symlink, and it requires
// owner-only bits on private material. A Secret volume satisfies neither
// condition:
//
//   - Mounted whole, it is a symlink farm. The kubelet writes the data into a
//     timestamped directory and projects each key as a symlink through
//     "..data". securefile refuses every one of those paths.
//   - Mounted one file at a time with subPath, the files are real, but the
//     kubelet owns them as root. The pod runs as a nonroot user, and no file
//     mode makes a root-owned file readable by that user without also setting
//     a group or other bit that securefile refuses on private material.
//
// So the material must be copied. An init container runs Install with the same
// user as the service, before the service starts. The service then meets the
// layout that securefile accepts, and no securefile rule changes.
//
// The compose stack solves the same problem in the same way. Read
// deploy/runtime/install-material.sh.
//
// # One copy engine, two planes
//
// The runtime plane (issue #404) and the authentication plane (issue #444)
// both need this. They have different files, and they read their paths from
// different sources, but the copy is the same operation. It lives here once.
// A second copy of this code would let one plane keep a defect that the other
// plane already fixed.
package materialinstall

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/security/securefile"
)

const (
	// One material file is a certificate bundle at most. The largest bound that
	// any reader applies is one mebibyte, so a larger source file could never be
	// read even after a good copy.
	maxMaterialFileBytes = 1 << 20
	// A Secret that holds more entries than this is not deployment material.
	maxMaterialEntries = 64
	// The mode that securefile accepts for private material, and that the
	// public-material profile also accepts.
	materialFileMode = 0o600
	// The kubelet writes its own projection bookkeeping under names that start
	// with two dots. A Secret key can never start with two dots, so this prefix
	// hides no operator data.
	projectionPrefix = ".."
)

// File is one file that the service opens at start, with the permission profile
// that its reader applies.
type File struct {
	Path        string
	Permissions securefile.Permissions
}

// Install copies the material and then proves that the service can read it.
//
// It writes one real file for each key of source into destination. It removes
// anything in destination that source does not carry. It then reads every
// required file back through securefile, with the profile the service applies,
// and fails if any read fails.
//
// So a missing Secret key, a truncated file or a wrong mode stops the pod in
// the init container, with a message, instead of in a restart loop of the
// service container.
//
// Install returns the destination paths that it wrote, in a stable order.
func Install(source, destination string, required []File) ([]string, error) {
	if len(required) == 0 {
		return nil, errors.New("the caller named no required material file")
	}
	if err := RequireDirectory(destination); err != nil {
		return nil, fmt.Errorf("the material directory %s is not usable: %w", destination, err)
	}
	written, err := copyTree(source, destination)
	if err != nil {
		return nil, err
	}
	if err := verify(required); err != nil {
		return nil, err
	}
	return written, nil
}

// copyTree writes one real file for each key of the source directory.
//
// It resolves each entry and keeps only the ones that stay inside the source
// directory. A symlink that leaves the mount is refused, which is the property
// that securefile enforces for the service itself.
func copyTree(source, destination string) ([]string, error) {
	root, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, fmt.Errorf("resolve the material source %s: %w", source, err)
	}
	if err := RequireDirectory(root); err != nil {
		return nil, fmt.Errorf("the material source %s is not usable: %w", source, err)
	}
	if root == destination {
		return nil, fmt.Errorf("the material source and the material directory are both %s, and the copy needs two directories", destination)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read the material source %s: %w", source, err)
	}
	if len(entries) > maxMaterialEntries {
		return nil, fmt.Errorf("the material source %s holds %d entries, and %d is the limit", source, len(entries), maxMaterialEntries)
	}

	written := make([]string, 0, len(entries))
	installed := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, projectionPrefix) {
			continue
		}
		if name == "" || name != filepath.Base(name) || strings.ContainsRune(name, os.PathSeparator) {
			return nil, fmt.Errorf("the material source holds an unusable entry name %q", name)
		}
		resolved, err := resolveInside(root, name)
		if err != nil {
			return nil, err
		}
		if resolved == "" {
			continue
		}
		target := filepath.Join(destination, name)
		if err := copyFile(resolved, target); err != nil {
			return nil, fmt.Errorf("install %s: %w", name, err)
		}
		written = append(written, target)
		installed[name] = struct{}{}
	}
	if len(written) == 0 {
		return nil, fmt.Errorf("the material source %s holds no key", source)
	}
	if err := prune(destination, installed); err != nil {
		return nil, err
	}
	sort.Strings(written)
	return written, nil
}

// prune deletes anything in the destination that this run did not install.
//
// The destination must mirror the source, and nothing else. Without this, a
// file that an earlier run left behind would still satisfy the check below,
// so a key that the operator removed from the Secret, or replaced with a name
// that no longer matches, would keep serving its old contents.
func prune(destination string, installed map[string]struct{}) error {
	entries, err := os.ReadDir(destination)
	if err != nil {
		return fmt.Errorf("read the material directory %s: %w", destination, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if _, keep := installed[name]; keep {
			continue
		}
		if entry.IsDir() {
			return fmt.Errorf("the material directory %s holds a subdirectory %s, which this command did not create", destination, name)
		}
		if err := os.Remove(filepath.Join(destination, name)); err != nil {
			return fmt.Errorf("remove the stale material file %s: %w", name, err)
		}
	}
	return nil
}

// resolveInside resolves one source entry and keeps it only when it is a
// regular file that stays inside root. It returns an empty path for a
// directory, which the kubelet also projects, and an error for a symlink that
// escapes root.
func resolveInside(root, name string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, name))
	if err != nil {
		return "", fmt.Errorf("resolve the material key %s: %w", name, err)
	}
	if resolved != filepath.Clean(resolved) || !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("the material key %s resolves to %s, which is outside the source directory %s", name, resolved, root)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("read the material key %s: %w", name, err)
	}
	if info.IsDir() {
		return "", nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the material key %s is not a regular file", name)
	}
	return resolved, nil
}

func copyFile(source, destination string) error {
	contents, err := readBounded(source)
	if err != nil {
		return err
	}
	defer clear(contents)

	// Remove first, so a rerun of this container replaces the file instead of
	// writing through a link that something else created in the shared volume.
	if err := os.Remove(destination); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, materialFileMode)
	if err != nil {
		return err
	}
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// The process umask masks the mode that OpenFile requests, so set the mode
	// again. securefile compares the exact bits, and a masked 0600 would still
	// be 0600 only by luck of the environment.
	return os.Chmod(destination, materialFileMode)
}

func readBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, maxMaterialFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(contents) == 0 {
		return nil, errors.New("the source file is empty")
	}
	if len(contents) > maxMaterialFileBytes {
		return nil, fmt.Errorf("the source file is larger than %d bytes", maxMaterialFileBytes)
	}
	return contents, nil
}

// verify reads every required file through the same reader and the same
// permission profile that the service applies. This is the gate: the pod stops
// here, with a message, rather than in a restart loop of the service container.
func verify(required []File) error {
	for _, file := range required {
		contents, err := securefile.Read(file.Path, maxMaterialFileBytes, file.Permissions)
		if err != nil {
			return fmt.Errorf("the service cannot read %s after the copy: %w", file.Path, err)
		}
		clear(contents)
	}
	return nil
}

// RequireDirectory reports whether the path is an existing directory.
func RequireDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("the path is not a directory")
	}
	return nil
}
