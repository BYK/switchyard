package switchyard

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// inputs is the manifest verified before execution, not a manifest supplied by
// the command. Compare content rather than trusting checkout mtimes or inodes.
// Git only receives frozen versions of changed files, so unchanged large files
// never become temporary Git objects. No shared index/cache needs invalidation.
func collectPatch(original, checkout, dest string, inputs []fileEntry, outputs ...string) string {
	before := make(map[string]fileEntry, len(inputs))
	for _, entry := range inputs {
		if safeRel(entry.Path) != nil || excludedPath(entry.Path) || requireSnapshot(entry.Hash) != nil ||
			entry.Size < 0 || entry.Size > maxFileSize || entry.Mode != 0o644 && entry.Mode != 0o755 {
			return "invalid patch baseline"
		}
		if _, exists := before[entry.Path]; exists {
			return "invalid patch baseline"
		}
		before[entry.Path] = entry
	}
	if validateOutputInputs(outputs, inputs) != nil {
		return "artifact roots overlap source inputs"
	}

	current := make(map[string]bool)
	var candidates []string
	var total int64
	err := filepath.WalkDir(checkout, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(checkout, path)
		rel = filepath.ToSlash(rel)
		if err != nil {
			return err
		}
		if excludedPath(rel) || withinOutput(rel, outputs) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if rel != "." && safeRel(rel) != nil {
			return errors.New("unsupported result path")
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileSize {
			return errors.New("unsupported result file")
		}
		total += info.Size()
		if total > maxSnapshotSize {
			return errors.New("result exceeds size limit")
		}
		current[rel] = true
		if _, exists := before[rel]; !exists {
			// check-ignore rejects pathspec magic, including literal magic.
			// A ./ prefix makes leading colons ordinary filename characters.
			candidates = append(candidates, "./"+rel)
		}
		return nil
	})
	if err != nil {
		return "worker edits include an unsupported file or exceed the size limit"
	}

	var stage, gitDir, index string
	defer func() {
		if stage != "" {
			os.RemoveAll(stage)
		}
	}()
	prepare := func() error {
		if stage != "" {
			return nil
		}
		var err error
		stage, err = os.MkdirTemp(filepath.Dir(dest), ".patch-collect-")
		if err != nil {
			return err
		}
		gitDir, index = filepath.Join(stage, "git"), filepath.Join(stage, "index")
		cmd := exec.Command("git", "init", "--bare", "--quiet", "--template=", gitDir)
		cmd.Env = patchEnvironment(index)
		if err := cmd.Run(); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0o700); err != nil {
			return err
		}
		return configurePatchAttributes(gitDir)
	}
	// Ignore rules only apply to new paths. A source file explicitly captured
	// despite .gitignore remains tracked, even if the command changes the rules.
	if len(candidates) != 0 {
		if err := prepare(); err != nil {
			return "could not prepare patch collection"
		}
		cmd := exec.Command("git", "--git-dir="+gitDir, "--work-tree="+checkout,
			"check-ignore", "--no-index", "-z", "--stdin")
		cmd.Dir, cmd.Env = checkout, patchEnvironment(index)
		names := strings.Join(candidates, "\x00") + "\x00"
		cmd.Stdin = strings.NewReader(names)
		response := &responseBuffer{limit: len(names)}
		cmd.Stdout = response
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				return "could not filter ignored worker files"
			}
		}
		if response.tooLarge {
			return "could not filter ignored worker files"
		}
		for _, path := range bytes.Split(response.data.Bytes(), []byte{0}) {
			delete(current, strings.TrimPrefix(string(path), "./"))
		}
	}

	var oldFiles, newFiles []fileEntry
	for path := range current {
		hash, info, err := digestFile(checkout, path)
		if err != nil {
			return "could not inspect worker edits"
		}
		entry := fileEntry{Path: path, Hash: hash, Size: info.Size(), Mode: normalizedMode(info.Mode())}
		if old, exists := before[path]; exists {
			if old == entry {
				continue
			}
			oldFiles = append(oldFiles, old)
		}
		newFiles = append(newFiles, entry)
	}
	for path, entry := range before {
		if !current[path] {
			oldFiles = append(oldFiles, entry)
		}
	}
	if len(oldFiles) == 0 && len(newFiles) == 0 {
		file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "could not create worker patch"
		}
		if err := file.Close(); err != nil {
			os.Remove(dest)
			return "could not save worker patch"
		}
		return ""
	}
	if err := prepare(); err != nil {
		return "could not prepare patch collection"
	}
	oldTree, newTree := filepath.Join(stage, "before"), filepath.Join(stage, "after")
	if err := freezePatchFiles(original, oldTree, oldFiles); err != nil {
		return "patch baseline changed during collection"
	}
	if err := freezePatchFiles(checkout, newTree, newFiles); err != nil {
		return "worker edits changed during collection"
	}
	if _, err := gitWithIndex(index, gitDir, oldTree, "add", "--force", "-A", "--", "."); err != nil {
		return "could not stage original changes"
	}
	baseline, err := gitWithIndex(index, gitDir, oldTree, "write-tree")
	if err != nil {
		return "could not record original changes"
	}
	if _, err := gitWithIndex(index, gitDir, newTree, "read-tree", "--empty"); err != nil {
		return "could not compare worker edits"
	}
	if _, err := gitWithIndex(index, gitDir, newTree, "add", "--force", "-A", "--", "."); err != nil {
		return "could not stage worker edits"
	}
	return exportPatch(index, gitDir, newTree, dest, strings.TrimSpace(string(baseline)))
}

func freezePatchFiles(source, dest string, entries []fileEntry) error {
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	for _, entry := range entries {
		if err := checkNoSymlinks(source, entry.Path); err != nil {
			return err
		}
		path := filepath.Join(source, filepath.FromSlash(entry.Path))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size || normalizedMode(info.Mode()) != entry.Mode {
			return errors.New("patch input changed")
		}
		if err := copyFile(path, filepath.Join(dest, filepath.FromSlash(entry.Path)), os.FileMode(entry.Mode)); err != nil {
			return err
		}
		hash, info, err := digestFile(dest, entry.Path)
		if err != nil || hash != entry.Hash || info.Size() != entry.Size || normalizedMode(info.Mode()) != entry.Mode {
			return errors.New("patch input changed")
		}
	}
	return nil
}

func exportPatch(index, gitDir, checkout, dest, tree string) string {
	cmd := exec.Command("git", "--git-dir="+gitDir, "--work-tree="+checkout,
		"diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", "--no-renames", tree)
	cmd.Dir, cmd.Env = checkout, patchEnvironment(index)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "could not collect worker patch"
	}
	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "could not create worker patch"
	}
	if err := cmd.Start(); err != nil {
		file.Close()
		os.Remove(dest)
		return "could not collect worker patch"
	}
	written, copyErr := io.Copy(file, io.LimitReader(pipe, maxPatchSize+1))
	if written > maxPatchSize || copyErr != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	closeErr := file.Close()
	if written > maxPatchSize || copyErr != nil || waitErr != nil || closeErr != nil {
		os.Remove(dest)
		return "worker edits exceed the patch limit or cannot be exported"
	}
	return ""
}
