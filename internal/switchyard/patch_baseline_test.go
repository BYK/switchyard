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

// The collector from merged PR #14, retained only as a benchmark and regression
// oracle. It is never compiled into the Switchyard binary.
func collectPatchFullTreeBaseline(original, checkout, dest string, _ []fileEntry, outputs ...string) string {
	var total int64
	if err := filepath.WalkDir(checkout, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(checkout, path)
		if err != nil {
			return err
		}
		if excludedPath(filepath.ToSlash(rel)) || withinOutput(filepath.ToSlash(rel), outputs) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileSize {
			return errors.New("worker produced an unsupported edit")
		}
		total += info.Size()
		if total > maxSnapshotSize {
			return errors.New("worker edits exceed the checkout size limit")
		}
		return nil
	}); err != nil {
		return "worker edits include an unsupported file or exceed the size limit"
	}
	gitDir := filepath.Join(filepath.Dir(checkout), "patch.git")
	index := filepath.Join(filepath.Dir(checkout), "patch-index")
	init := exec.Command("git", "init", "--bare", "--quiet", "--template=", gitDir)
	init.Env = patchEnvironment(index)
	if err := init.Run(); err != nil {
		return "could not prepare patch collection"
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0o700); err != nil {
		return "could not configure patch collection"
	}
	if err := configurePatchAttributes(gitDir); err != nil {
		return "could not configure patch collection"
	}
	if _, err := gitWithIndex(index, gitDir, original, "read-tree", "--empty"); err != nil {
		return "could not prepare patch collection"
	}
	if _, err := gitWithIndex(index, gitDir, original, "add", "--force", "-A", "--", "."); err != nil {
		return "could not stage original snapshot"
	}
	baseline, err := gitWithIndex(index, gitDir, original, "write-tree")
	if err != nil {
		return "could not record original snapshot"
	}
	tree := strings.TrimSpace(string(baseline))
	if _, err := gitWithIndex(index, gitDir, checkout, "read-tree", tree); err != nil {
		return "could not compare worker edits"
	}
	// Git must ignore generated caches and private paths before staging: removing
	// them from the index afterward would still write their bytes to Git objects.
	patterns := make([]string, 0, len(deniedParts)+len(deniedGlobs))
	for part := range deniedParts {
		patterns = append(patterns, "**/"+part)
	}
	for _, glob := range deniedGlobs {
		patterns = append(patterns, "**/"+glob)
	}
	for _, output := range outputs {
		patterns = append(patterns, "/"+output)
	}
	sort.Strings(patterns)
	if err := writeAtomic(filepath.Join(gitDir, "info", "exclude"), []byte(strings.Join(patterns, "\n")+"\n")); err != nil {
		return "could not exclude private worker files"
	}
	if _, err := gitWithIndex(index, gitDir, checkout, "add", "-A", "--", "."); err != nil {
		return "could not stage worker edits"
	}
	paths, err := gitWithIndex(index, gitDir, checkout, "ls-files", "--cached", "-z")
	if err != nil {
		return "could not filter worker edits"
	}
	private := make([]string, 0)
	for _, path := range bytes.Split(paths, []byte{0}) {
		if len(path) > 0 && excludedPath(string(path)) {
			private = append(private, string(path))
		}
	}
	for len(private) > 0 {
		count := len(private)
		if count > 100 {
			count = 100
		}
		args := append([]string{"rm", "--cached", "-q", "-f", "--ignore-unmatch", "--"}, private[:count]...)
		if _, err := gitWithIndex(index, gitDir, checkout, args...); err != nil {
			return "could not exclude private worker files"
		}
		private = private[count:]
	}
	cmd := exec.Command("git", "--git-dir="+gitDir, "--work-tree="+checkout,
		"diff", "--cached", "--binary", "--no-ext-diff", tree)
	cmd.Dir = checkout
	cmd.Env = patchEnvironment(index)
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
