package switchyard

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

const (
	maxArtifactBytes    = 64 << 20
	maxArtifactBundle   = maxArtifactBytes + (4 << 20)
	maxArtifactEntries  = 4096
	maxArtifactManifest = 1 << 20
	artifactFile        = "artifacts.tar"
)

type artifactEntry struct {
	fileEntry
	Kind string `json:"kind"`
}

type artifactManifest struct {
	Version int             `json:"version"`
	Outputs []string        `json:"outputs"`
	Entries []artifactEntry `json:"entries"`
}

func withinOutput(path string, outputs []string) bool {
	for _, output := range outputs {
		if path == output || strings.HasPrefix(path, output+"/") {
			return true
		}
	}
	return false
}

func normalizeOutputs(paths []string) ([]string, error) {
	if len(paths) > 32 {
		return nil, errors.New("at most 32 output paths may be declared")
	}
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSuffix(path, "/")
		if safeRel(path) != nil || excludedPath(path) || len(path) > 4096 || strings.TrimSpace(path) != path || strings.ContainsAny(path, "*?[]") {
			return nil, errors.New("outputs must be exact non-private paths relative to the worktree root")
		}
		result = append(result, path)
	}
	sort.Strings(result)
	for i, path := range result {
		if withinOutput(path, result[:i]) {
			return nil, errors.New("output paths must not overlap or repeat")
		}
	}
	return result, nil
}

func declaredOutputs(flags []string) ([]string, error) {
	var paths []string
	if value := os.Getenv("SWITCHYARD_OUTPUTS"); value != "" {
		if len(value) > 16384 || json.Unmarshal([]byte(value), &paths) != nil || paths == nil {
			return nil, errors.New("SWITCHYARD_OUTPUTS must be a JSON array of output paths")
		}
	}
	return normalizeOutputs(append(paths, flags...))
}

func validateOutputInputs(outputs []string, files []fileEntry) error {
	for _, entry := range files {
		if withinOutput(entry.Path, outputs) {
			return errors.New("declared outputs overlap selected source inputs")
		}
		for _, output := range outputs {
			if strings.HasPrefix(output, entry.Path+"/") {
				return errors.New("an output parent is a selected input file")
			}
		}
	}
	return nil
}

// Existing outputs are a local conflict baseline, never input to the worker.
// Missing roots are allowed only in that baseline. A completed result must
// contain every declared root, including an explicit directory for empty output.
func inspectArtifacts(root string, outputs []string, allowMissing bool) (artifactManifest, error) {
	m := artifactManifest{Version: 1, Outputs: outputs}
	var total int64
	for _, output := range outputs {
		if err := checkReturnPath(root, output); err != nil {
			return m, errors.New("output path contains an unsafe ancestor")
		}
		target := filepath.Join(root, filepath.FromSlash(output))
		if _, err := os.Lstat(target); os.IsNotExist(err) && allowMissing {
			m.Entries = append(m.Entries, artifactEntry{fileEntry: fileEntry{Path: output}, Kind: "missing"})
			continue
		}
		err := filepath.WalkDir(target, func(path string, item os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return errors.New("a declared output is missing or unreadable")
			}
			rel, err := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if err != nil || safeRel(rel) != nil || excludedPath(rel) || len(rel) > 4096 {
				return errors.New("output includes a private or unsupported path")
			}
			if len(m.Entries) >= maxArtifactEntries {
				return errors.New("outputs exceed the entry limit")
			}
			entry := artifactEntry{fileEntry: fileEntry{Path: rel}, Kind: "directory"}
			if item.IsDir() {
				entry.Mode = 0o755
				if allowMissing {
					info, err := item.Info()
					if err != nil {
						return err
					}
					entry.Mode = uint32(info.Mode().Perm())
				}
			} else {
				if !item.Type().IsRegular() {
					return errors.New("outputs must contain only regular files and directories")
				}
				hash, info, err := digestFile(root, rel)
				if err != nil {
					return err
				}
				entry.Kind, entry.Hash, entry.Size, entry.Mode = "file", hash, info.Size(), normalizedMode(info.Mode())
				if allowMissing {
					// Compare all local permissions, although transported files use
					// normalized modes just like selected source inputs.
					entry.Mode = uint32(info.Mode().Perm())
				}
				total += entry.Size
				if total > maxArtifactBytes {
					return errors.New("outputs exceed the 64 MiB content limit")
				}
			}
			m.Entries = append(m.Entries, entry)
			return nil
		})
		if err != nil {
			return m, err
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m, nil
}

type artifactWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *artifactWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > w.remaining {
		return 0, errors.New("artifact archive exceeds its size limit")
	}
	n, err := w.writer.Write(data)
	w.remaining -= int64(n)
	return n, err
}

// Stage a complete archive before exposing it to a client or reclaiming the
// checkout. Both the manifest and every streamed file are checked for changes.
func collectArtifacts(root, dest string, outputs []string) (string, int64, error) {
	m, err := inspectArtifacts(root, outputs, false)
	if err != nil {
		return "", 0, err
	}
	metadata, err := json.Marshal(m)
	if err != nil || len(metadata) > maxArtifactManifest {
		return "", 0, errors.New("artifact manifest exceeds its size limit")
	}
	file, err := os.CreateTemp(filepath.Dir(dest), ".artifacts-")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	bounded := &artifactWriter{writer: io.MultiWriter(file, hash), remaining: maxArtifactBundle}
	w := tar.NewWriter(bounded)
	if err := w.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(metadata))}); err != nil {
		return "", 0, err
	}
	if _, err := w.Write(metadata); err != nil {
		return "", 0, err
	}
	for _, entry := range m.Entries {
		if entry.Kind != "file" {
			continue
		}
		if err := w.WriteHeader(&tar.Header{Name: "files/" + entry.Path, Typeflag: tar.TypeReg, Mode: int64(entry.Mode), Size: entry.Size}); err != nil {
			return "", 0, err
		}
		input, info, err := openRegular(root, entry.Path)
		if err != nil {
			return "", 0, err
		}
		digest := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(w, digest), io.LimitReader(input, entry.Size+1))
		input.Close()
		if copyErr != nil || n != entry.Size || info.Size() != entry.Size || normalizedMode(info.Mode()) != entry.Mode || hex.EncodeToString(digest.Sum(nil)) != entry.Hash {
			return "", 0, errors.New("outputs changed during collection")
		}
	}
	after, err := inspectArtifacts(root, outputs, false)
	if err != nil || !reflect.DeepEqual(m, after) {
		return "", 0, errors.New("outputs changed during collection")
	}
	if err := w.Close(); err != nil {
		return "", 0, err
	}
	if err := file.Sync(); err != nil {
		return "", 0, err
	}
	if err := file.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(file.Name(), dest); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), maxArtifactBundle - bounded.remaining, nil
}

// The manifest is the allowlist: never extract arbitrary archive names, links,
// special files, duplicate members, or bytes beyond the declared content.
func unpackArtifacts(data []byte, expectedHash string, outputs []string, dest string) error {
	actualHash := sha256.Sum256(data)
	if len(data) == 0 || len(data) > maxArtifactBundle || hex.EncodeToString(actualHash[:]) != expectedHash {
		return errors.New("artifact archive is incomplete or has a different digest")
	}
	reader := bytes.NewReader(data)
	r := tar.NewReader(reader)
	header, err := r.Next()
	if err != nil || header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Size < 0 || header.Size > maxArtifactManifest {
		return errors.New("invalid artifact manifest header")
	}
	metadata, err := io.ReadAll(io.LimitReader(r, maxArtifactManifest+1))
	var m artifactManifest
	if err != nil || json.Unmarshal(metadata, &m) != nil || m.Version != 1 || !equalPaths(m.Outputs, outputs) || len(m.Entries) == 0 || len(m.Entries) > maxArtifactEntries {
		return errors.New("invalid artifact manifest")
	}
	seen := map[string]string{}
	var total int64
	for i, entry := range m.Entries {
		if safeRel(entry.Path) != nil || excludedPath(entry.Path) || len(entry.Path) > 4096 || !withinOutput(entry.Path, outputs) || i > 0 && m.Entries[i-1].Path >= entry.Path {
			return errors.New("artifact manifest includes an unexpected path")
		}
		isRoot := false
		for _, output := range outputs {
			if entry.Path == output {
				isRoot = true
			}
		}
		if !isRoot && seen[filepath.ToSlash(filepath.Dir(entry.Path))] != "directory" {
			return errors.New("artifact manifest is missing a parent directory")
		}
		target := filepath.Join(dest, filepath.FromSlash(entry.Path))
		if entry.Kind == "directory" {
			if entry.Size != 0 || entry.Hash != "" || entry.Mode != 0o755 {
				return errors.New("invalid output directory")
			}
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		} else if entry.Kind == "file" {
			if entry.Size < 0 || entry.Size > maxArtifactBytes || entry.Mode != 0o644 && entry.Mode != 0o755 || requireSnapshot(entry.Hash) != nil {
				return errors.New("invalid artifact file")
			}
			total += entry.Size
			if total > maxArtifactBytes {
				return errors.New("artifact contents exceed their size limit")
			}
			header, err := r.Next()
			if err != nil || header.Name != "files/"+entry.Path || header.Typeflag != tar.TypeReg || header.Size != entry.Size || header.Mode != int64(entry.Mode) {
				return errors.New("artifact archive differs from its manifest")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(entry.Mode))
			if err != nil {
				return err
			}
			hash := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(file, hash), r)
			modeErr := file.Chmod(os.FileMode(entry.Mode))
			closeErr := file.Close()
			if copyErr != nil || modeErr != nil || closeErr != nil || n != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.Hash {
				return errors.New("artifact file is incomplete or has a different digest")
			}
		} else {
			return errors.New("unsupported artifact file type")
		}
		seen[entry.Path] = entry.Kind
	}
	for _, output := range outputs {
		if seen[output] == "" {
			return errors.New("artifact archive is missing a declared output")
		}
	}
	if _, err := r.Next(); err != io.EOF || reader.Len() != 0 {
		return errors.New("artifact archive has unexpected trailing data")
	}
	return nil
}
