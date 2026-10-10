package switchyard

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"
)

const (
	maxFileSize     = 128 << 20
	maxSnapshotSize = 2 << 30
	maxPatchSize    = 64 << 20
	maxLogSize      = 128 << 20
	maxWireRequest  = 2 << 20
	manifestName    = ".switchyard-manifest.json"
)

var (
	idPattern       = regexp.MustCompile(`^[0-9a-f]{32}$`)
	snapshotPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	remotePath      = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	remoteName      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	remoteUser      = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
)

func requireID(id string) error {
	if !idPattern.MatchString(id) {
		return errors.New("invalid job or project ID")
	}
	return nil
}

func requireSnapshot(id string) error {
	if !snapshotPattern.MatchString(id) {
		return errors.New("invalid snapshot ID")
	}
	return nil
}

func validateEnvironment(environment []string) error {
	seen := make(map[string]bool, len(environment))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsRune(entry, 0) || seen[name] {
			return errors.New("invalid invocation environment")
		}
		seen[name] = true
	}
	return nil
}

func workerEnvironment(environment []string, cwd string) []string {
	result := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if name != "PWD" && name != "SWITCHYARD_BACKEND" {
			result = append(result, entry)
		}
	}
	return append(result, "PWD="+cwd, "SWITCHYARD_BACKEND=local")
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errors.New("could not create job ID")
	}
	return hex.EncodeToString(bytes[:]), nil
}

func dataHome() (string, error) {
	if override := os.Getenv("SWITCHYARD_HOME"); override != "" {
		return filepath.Abs(override)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.New("could not locate the home directory")
	}
	return filepath.Join(home, ".local", "share", "switchyard"), nil
}

func privateDir(path string) error {
	if info, err := os.Lstat(path); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid())) {
		return errors.New("private directory has an invalid owner or type")
	} else if err != nil && !os.IsNotExist(err) {
		return errors.New("could not inspect private directory")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return errors.New("could not create private directory")
	}
	return os.Chmod(path, 0o700)
}

func privateTree(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return errors.New("invalid private directory")
	}
	for parent := path; ; parent = filepath.Dir(parent) {
		if info, err := os.Lstat(parent); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return errors.New("private directory contains a symbolic link")
		} else if err != nil && !os.IsNotExist(err) {
			return errors.New("could not inspect private directory")
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	return privateDir(path)
}

func writeAtomic(path string, content []byte) error {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
		return errors.New("private file has an invalid type")
	} else if err != nil && !os.IsNotExist(err) {
		return errors.New("could not inspect private file")
	}
	file, err := os.CreateTemp(filepath.Dir(path), "record-*")
	if err != nil {
		return errors.New("could not create private file")
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func privateRead(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > limit || info.Mode().Perm()&0o077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, errors.New("private file is missing or invalid")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("could not read private file")
	}
	return content, nil
}

func runOutput(name string, args ...string) ([]byte, error) {
	return runOutputAt("", name, args...)
}

func runOutputAt(directory, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	if directory != "" {
		cmd.Dir = directory
	}
	output, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%s failed (exit %d)", filepath.Base(name), exit.ExitCode())
		}
		return nil, fmt.Errorf("could not run %s", filepath.Base(name))
	}
	return output, nil
}

func safeRel(path string) error {
	if path == "" || path == "." || strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\\x00") || !utf8.ValidString(path) {
		return errors.New("invalid worktree path")
	}
	for _, char := range path {
		if unicode.IsControl(char) {
			return errors.New("worktree path contains control characters")
		}
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." {
			return errors.New("invalid worktree path")
		}
	}
	return nil
}

func copyFile(source, dest string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func copyTree(source, dest string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errors.New("only regular files can be copied")
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}
