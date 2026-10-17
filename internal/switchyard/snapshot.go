package switchyard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"
)

type fileEntry struct {
	Path string `json:"path"`
	Hash string `json:"sha256"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
}

type manifest struct {
	Version  int         `json:"version"`
	Files    []fileEntry `json:"files"`
	Snapshot string      `json:"snapshot"`
}

type selection struct {
	Root     string
	CWD      string
	VCS      string
	Paths    []string
	Excluded []string
}

type capture struct {
	Tree     string
	Snapshot string
	Manifest manifest
}

var deniedParts = map[string]bool{
	".git": true, ".jj": true, ".aws": true, ".azure": true, ".kube": true,
	".docker": true, ".gnupg": true, ".terraform": true, ".ssh": true,
	".opencode": true, "node_modules": true, ".pnpm-store": true, ".venv": true, ".cache": true,
	".lore.md": true, ".npmrc": true, ".pypirc": true, ".netrc": true,
	".envrc": true, "credentials": true, "credentials.json": true,
	"secrets.json": true, "service-account.json": true, "id_ed25519": true,
	"id_rsa": true, manifestName: true,
}

var deniedGlobs = []string{
	".switchyard-return-*",
	".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "*.sqlite",
	"*.sqlite3", "*.db", "*.tfstate", "*.tfstate.*", "*.tfvars", "*.tfvars.*",
}

func excludedPath(path string) bool {
	for _, part := range strings.Split(path, "/") {
		if deniedParts[part] {
			return true
		}
		for _, glob := range deniedGlobs {
			if matched, _ := filepath.Match(glob, part); matched {
				return true
			}
		}
	}
	return false
}

func locateRepo(start string) (string, string, string, error) {
	start, err := filepath.Abs(start)
	if err != nil {
		return "", "", "", errors.New("invalid worktree directory")
	}
	start, err = filepath.EvalSymlinks(start)
	if err != nil {
		return "", "", "", errors.New("worktree directory does not exist")
	}
	for parent := start; ; parent = filepath.Dir(parent) {
		for _, vcs := range []string{"jj", "git"} {
			if _, err := os.Lstat(filepath.Join(parent, "."+vcs)); err == nil {
				relative, err := filepath.Rel(parent, start)
				return parent, vcs, filepath.ToSlash(relative), err
			} else if !os.IsNotExist(err) {
				return "", "", "", errors.New("could not inspect worktree")
			}
		}
		if filepath.Dir(parent) == parent {
			break
		}
	}
	return "", "", "", errors.New("run inside a Git or jj worktree")
}

func fileList(root, vcs string) ([]string, error) {
	var output []byte
	var err error
	switch vcs {
	case "git":
		var unmerged []byte
		unmerged, err = runOutput("git", "-C", root, "ls-files", "--unmerged", "-z")
		if err == nil && len(unmerged) > 0 {
			return nil, errors.New("resolve Git index conflicts before capture")
		}
		if err == nil {
			output, err = runOutput("git", "-C", root, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
		}
	case "jj":
		var conflicted []byte
		conflicted, err = runOutputAt(root, "jj", "-R", root, "log", "-r", "conflicts() & @", "-T", "commit_id", "--no-pager")
		if err == nil && len(conflicted) > 0 {
			return nil, errors.New("resolve jj conflicts before capture")
		}
		if err == nil {
			output, err = runOutputAt(root, "jj", "-R", root, "file", "list", "-r", "@", "-T", "path.display() ++ \"\\0\"", "--no-pager")
		}
	default:
		return nil, errors.New("unsupported worktree type")
	}
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		if !utf8.Valid(raw) {
			return nil, errors.New("worktree contains a path that is not UTF-8")
		}
		path := string(raw)
		if err := safeRel(path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func selectFiles(start string, includes []string) (selection, error) {
	root, vcs, cwd, err := locateRepo(start)
	if err != nil {
		return selection{}, err
	}
	paths, err := fileList(root, vcs)
	if err != nil {
		return selection{}, err
	}
	seen := make(map[string]bool)
	for _, path := range paths {
		seen[path] = true
	}
	for _, path := range includes {
		if err := safeRel(path); err != nil || excludedPath(path) {
			return selection{}, fmt.Errorf("cannot include a private or invalid path: %q", path)
		}
		seen[path] = true
	}
	// Git and jj hide ignored paths, including common private root files.
	// List only root entries here; recursively walking dependency trees would
	// make every preview slower and is not needed to prevent transfer.
	entries, err := os.ReadDir(root)
	if err != nil {
		return selection{}, errors.New("could not inspect worktree root")
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != ".git" && name != ".jj" && excludedPath(name) {
			seen[name] = true
		}
	}
	result := selection{Root: root, CWD: cwd, VCS: vcs}
	for path := range seen {
		if excludedPath(path) {
			result.Excluded = append(result.Excluded, path)
			continue
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err == nil {
			result.Paths = append(result.Paths, path)
		} else if !os.IsNotExist(err) {
			return selection{}, errors.New("could not inspect a worktree file")
		}
	}
	sort.Strings(result.Paths)
	sort.Strings(result.Excluded)
	for _, path := range includes {
		if !seen[path] {
			return selection{}, errors.New("requested file was not selected")
		}
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			return selection{}, errors.New("requested file does not exist")
		}
	}
	return result, nil
}

func checkNoSymlinks(root, rel string) error {
	current := root
	for _, part := range strings.Split(rel, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("snapshot includes a symbolic link or missing file")
		}
	}
	return nil
}

func openRegular(root, path string) (*os.File, os.FileInfo, error) {
	if err := safeRel(path); err != nil {
		return nil, nil, err
	}
	if err := checkNoSymlinks(root, path); err != nil {
		return nil, nil, err
	}
	fd, err := syscall.Open(filepath.Join(root, filepath.FromSlash(path)), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, errors.New("file changed while capturing")
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileSize {
		file.Close()
		return nil, nil, errors.New("only regular files up to 128 MiB may be captured")
	}
	return file, info, nil
}

func digestFile(root, path string) (string, os.FileInfo, error) {
	file, info, err := openRegular(root, path)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxFileSize+1)); err != nil {
		return "", nil, errors.New("could not hash snapshot file")
	}
	return hex.EncodeToString(hash.Sum(nil)), info, nil
}

func snapshotHash(m manifest) (string, error) {
	m.Snapshot = ""
	encoded, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func captureFiles(start string, includes []string, home string) (result capture, chosen selection, captureErr error) {
	chosen, err := selectFiles(start, includes)
	if err != nil {
		return capture{}, selection{}, err
	}
	if err := privateTree(filepath.Join(home, "captures")); err != nil {
		return capture{}, selection{}, err
	}
	stage, err := os.MkdirTemp(filepath.Join(home, "captures"), "capture-")
	if err != nil {
		return capture{}, selection{}, errors.New("could not stage snapshot")
	}
	defer func() {
		if captureErr != nil {
			os.RemoveAll(stage)
		}
	}()
	tree := filepath.Join(stage, "files")
	if err := os.Mkdir(tree, 0o700); err != nil {
		return capture{}, selection{}, err
	}
	m := manifest{Version: 1}
	var total int64
	for _, path := range chosen.Paths {
		file, before, err := openRegular(chosen.Root, path)
		if err != nil {
			return capture{}, selection{}, fmt.Errorf("cannot capture %q: %w", path, err)
		}
		if total+before.Size() > maxSnapshotSize {
			file.Close()
			return capture{}, selection{}, errors.New("snapshot exceeds the 2 GiB limit")
		}
		dest := filepath.Join(tree, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			file.Close()
			return capture{}, selection{}, err
		}
		output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			file.Close()
			return capture{}, selection{}, err
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(output, hash), io.LimitReader(file, maxFileSize+1))
		closeErr := output.Close()
		after, statErr := file.Stat()
		file.Close()
		if copyErr != nil || closeErr != nil || statErr != nil || written != before.Size() || written > maxFileSize || !os.SameFile(before, after) || before.ModTime() != after.ModTime() || before.Mode() != after.Mode() {
			return capture{}, selection{}, errors.New("file changed while capturing; retry")
		}
		mode := os.FileMode(0o644)
		if before.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		if err := os.Chmod(dest, mode); err != nil {
			return capture{}, selection{}, err
		}
		m.Files = append(m.Files, fileEntry{Path: path, Size: written, Mode: uint32(mode), Hash: hex.EncodeToString(hash.Sum(nil))})
		total += written
	}
	current, err := selectFiles(start, includes)
	if err != nil || !equalPaths(chosen.Paths, current.Paths) || !equalPaths(chosen.Excluded, current.Excluded) {
		return capture{}, selection{}, errors.New("files changed during capture; retry")
	}
	for _, entry := range m.Files {
		hash, info, err := digestFile(chosen.Root, entry.Path)
		if err != nil || hash != entry.Hash || info.Size() != entry.Size || normalizedMode(info.Mode()) != entry.Mode {
			return capture{}, selection{}, errors.New("files changed during capture; retry")
		}
	}
	m.Snapshot, err = snapshotHash(m)
	if err != nil {
		return capture{}, selection{}, err
	}
	content, err := json.Marshal(m)
	if err != nil {
		return capture{}, selection{}, err
	}
	if err := os.WriteFile(filepath.Join(tree, manifestName), content, 0o600); err != nil {
		return capture{}, selection{}, err
	}
	return capture{Tree: tree, Snapshot: m.Snapshot, Manifest: m}, chosen, nil
}

func normalizedMode(mode os.FileMode) uint32 {
	if mode.Perm()&0o111 != 0 {
		return 0o755
	}
	return 0o644
}

func equalPaths(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
