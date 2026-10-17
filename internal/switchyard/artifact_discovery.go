package switchyard

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Source files and their ancestors cannot be replaced by an artifact root.
func artifactSourcePaths(inputs []fileEntry) map[string]bool {
	paths := map[string]bool{".": true}
	for _, input := range inputs {
		for path := input.Path; path != "."; path = filepath.ToSlash(filepath.Dir(path)) {
			paths[path] = true
		}
	}
	return paths
}

// The checkout starts with selected source files only. New ignored paths are
// generated outputs; ordinary new source files continue through source patches.
// An ignored directory with no source inputs is a complete replacement root.
func discoverArtifacts(root string, inputs []fileEntry) ([]string, error) {
	source := artifactSourcePaths(inputs)
	var candidates []string
	var namesSize int
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.New("could not discover generated outputs")
		}
		rel, err := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if err != nil {
			return err
		}
		if excludedPath(rel) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if source[rel] {
			return nil
		}
		if safeRel(rel) != nil || len(rel) > 4096 {
			return errors.New("generated output has an unsupported path")
		}
		candidates = append(candidates, rel)
		namesSize += len(rel) + 1
		if len(candidates) > maxArtifactEntries || namesSize > maxArtifactManifest {
			return errors.New("generated output discovery exceeds its limit")
		}
		return nil
	})
	if err != nil || len(candidates) == 0 {
		return nil, err
	}
	stage, err := os.MkdirTemp("", "switchyard-artifact-discovery-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	gitDir, index := filepath.Join(stage, "git"), filepath.Join(stage, "index")
	init := exec.Command("git", "init", "--bare", "--quiet", "--template=", gitDir)
	init.Env = patchEnvironment(index)
	if err := init.Run(); err != nil {
		return nil, errors.New("could not prepare output discovery")
	}
	cmd := exec.Command("git", "--git-dir="+gitDir, "--work-tree="+root, "check-ignore", "--no-index", "-z", "--stdin")
	cmd.Dir, cmd.Env = root, patchEnvironment(index)
	cmd.Stdin = strings.NewReader(strings.Join(candidates, "\x00") + "\x00")
	buffer := &responseBuffer{limit: maxArtifactManifest}
	cmd.Stdout = buffer
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, errors.New("could not discover ignored outputs")
		}
	}
	var roots []string
	if buffer.tooLarge {
		return nil, errors.New("output discovery response exceeds its limit")
	}
	for _, path := range bytes.Split(buffer.data.Bytes(), []byte{0}) {
		if len(path) != 0 && !withinOutput(string(path), roots) {
			roots = append(roots, string(path))
		}
	}
	return normalizeOutputRoots(roots, maxArtifactEntries)
}

// Output names are not known before dispatch. Capture eligible local files now
// so a later-discovered root can still be checked against its original contents.
// Never read private/cache paths. Unreadable/oversized paths are blocked rather
// than preventing an otherwise useful command from running.
type automaticArtifactBaseline struct {
	inputs  []fileEntry
	entries []artifactEntry
	blocked []string
	err     error
}

func captureAutomaticArtifacts(root string, inputs []fileEntry) *automaticArtifactBaseline {
	before := &automaticArtifactBaseline{inputs: inputs}
	source := artifactSourcePaths(inputs)
	var total int64
	before.err = filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
		rel, err := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if err != nil {
			return err
		}
		if len(before.entries)+len(before.blocked) >= maxArtifactEntries {
			return errors.New("local output baseline exceeds its entry limit")
		}
		if walkErr != nil || excludedPath(rel) {
			before.blocked = append(before.blocked, rel)
			if item != nil && item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if source[rel] {
			return nil
		}
		info, err := item.Info()
		if err != nil || safeRel(rel) != nil || len(rel) > 4096 || !info.IsDir() && !info.Mode().IsRegular() {
			before.blocked = append(before.blocked, rel)
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entry := artifactEntry{fileEntry: fileEntry{Path: rel, Mode: uint32(info.Mode().Perm())}, Kind: "directory"}
		if info.Mode().IsRegular() {
			if info.Size() > maxArtifactBytes-total {
				before.blocked = append(before.blocked, rel)
				return nil
			}
			hash, info, err := digestFile(root, rel)
			if err != nil || info.Size() > maxArtifactBytes-total {
				before.blocked = append(before.blocked, rel)
				return nil
			}
			total += info.Size()
			entry.Kind, entry.Hash, entry.Size, entry.Mode = "file", hash, info.Size(), uint32(info.Mode().Perm())
		}
		before.entries = append(before.entries, entry)
		return nil
	})
	return before
}

func (before *automaticArtifactBaseline) selectRoots(outputs []string) (artifactManifest, error) {
	m := artifactManifest{Version: 1, Outputs: outputs}
	if before.err != nil {
		return m, before.err
	}
	if err := validateOutputInputs(outputs, before.inputs); err != nil {
		return m, err
	}
	for _, path := range before.blocked {
		if path == "." || withinOutput(path, outputs) {
			return m, errArtifactConflict
		}
		for _, output := range outputs {
			if withinOutput(output, []string{path}) {
				return m, errArtifactConflict
			}
		}
	}
	seen := map[string]bool{}
	for _, entry := range before.entries {
		if withinOutput(entry.Path, outputs) {
			m.Entries = append(m.Entries, entry)
			seen[entry.Path] = true
		}
	}
	for _, output := range outputs {
		if !seen[output] {
			m.Entries = append(m.Entries, artifactEntry{fileEntry: fileEntry{Path: output}, Kind: "missing"})
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m, nil
}
