package switchyard

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func artifactWrite(t *testing.T, root, path, value string, mode os.FileMode) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	recoveryWrite(t, target, value, mode)
}

func artifactBundleFixture(t *testing.T) (string, string, int64) {
	t.Helper()
	root := t.TempDir()
	artifactWrite(t, root, "dist/new.bin", "\x00new output\xff", 0o755)
	artifactWrite(t, root, "report.txt", "complete report\n", 0o644)
	if err := os.Mkdir(filepath.Join(root, "dist", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), artifactFile)
	hash, size, err := collectArtifacts(root, path, []string{"dist", "report.txt"})
	if err != nil {
		t.Fatal(err)
	}
	return path, hash, size
}

func TestArtifactRoundTripReplacesCompleteRoots(t *testing.T) {
	root, home := fixture(t), t.TempDir()
	artifactWrite(t, root, ".gitignore", "dist/\nreport.txt\n.env\n", 0o644)
	artifactWrite(t, root, "dist/obsolete.txt", "stale", 0o644)
	artifactWrite(t, root, "report.txt/old-directory-child", "stale", 0o644)
	baseline, err := inspectArtifacts(root, []string{"dist", "report.txt"}, true)
	if err != nil {
		t.Fatal(err)
	}
	path, hash, size := artifactBundleFixture(t)
	data := resultRead(t, path)
	if int64(len(data)) != size {
		t.Fatal("incorrect archive size")
	}
	extracted := t.TempDir()
	if err := unpackArtifacts(data, hash, baseline.Outputs, extracted); err != nil {
		t.Fatal(err)
	}
	index := resultRead(t, filepath.Join(root, ".git", "index"))
	if err := publishArtifacts(home, root, extracted, baseline); err != nil {
		t.Fatal(err)
	}
	if got := string(resultRead(t, filepath.Join(root, "dist", "new.bin"))); got != "\x00new output\xff" {
		t.Fatalf("output bytes changed: %q", got)
	}
	if info, err := os.Stat(filepath.Join(root, "dist", "new.bin")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatal("executable bit lost")
	}
	if _, err := os.Stat(filepath.Join(root, "dist", "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatal("stale artifact survived replacement")
	}
	if info, err := os.Stat(filepath.Join(root, "dist", "empty")); err != nil || !info.IsDir() || info.Mode().Perm() != 0o755 {
		t.Fatal("empty directory or its permissions lost")
	}
	if string(resultRead(t, filepath.Join(root, "report.txt"))) != "complete report\n" {
		t.Fatal("directory-to-file replacement failed")
	}
	if !bytes.Equal(index, resultRead(t, filepath.Join(root, ".git", "index"))) {
		t.Fatal("artifact return changed the caller's index")
	}
	if string(resultRead(t, filepath.Join(root, "README.md"))) != "original\n" {
		t.Fatal("artifact return touched source")
	}
	stages, _ := filepath.Glob(filepath.Join(root, ".switchyard-return-*"))
	if len(stages) != 0 {
		t.Fatal("successful publication retained temporary trees")
	}
}

func TestArtifactConflictLeavesAllRootsUntouched(t *testing.T) {
	for _, change := range []string{"edit", "delete", "addition", "mode", "file permissions", "directory permissions", "symlink"} {
		t.Run(change, func(t *testing.T) {
			root, home := fixture(t), t.TempDir()
			artifactWrite(t, root, "dist/old", "old", 0o644)
			artifactWrite(t, root, "report.txt", "old report", 0o644)
			baseline, err := inspectArtifacts(root, []string{"dist", "report.txt"}, true)
			if err != nil {
				t.Fatal(err)
			}
			path, hash, _ := artifactBundleFixture(t)
			extracted := t.TempDir()
			if err := unpackArtifacts(resultRead(t, path), hash, baseline.Outputs, extracted); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "edit":
				artifactWrite(t, root, "report.txt", "local", 0o644)
			case "delete":
				if err := os.Remove(filepath.Join(root, "report.txt")); err != nil {
					t.Fatal(err)
				}
			case "addition":
				artifactWrite(t, root, "dist/local", "local", 0o644)
			case "mode":
				if err := os.Chmod(filepath.Join(root, "report.txt"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "file permissions":
				if err := os.Chmod(filepath.Join(root, "report.txt"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "directory permissions":
				if err := os.Chmod(filepath.Join(root, "dist"), 0o750); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(filepath.Join(root, "report.txt")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "report.txt")); err != nil {
					t.Fatal(err)
				}
			}
			if err := publishArtifacts(home, root, extracted, baseline); !errors.Is(err, errArtifactConflict) {
				t.Fatalf("expected conflict: %v", err)
			}
			if string(resultRead(t, filepath.Join(root, "dist", "old"))) != "old" {
				t.Fatal("conflict replaced another root")
			}
			if _, err := os.Stat(filepath.Join(root, "dist", "new.bin")); !os.IsNotExist(err) {
				t.Fatal("conflict partially published output")
			}
		})
	}
}

func TestArtifactEmptyDirectoryClearsStaleChildren(t *testing.T) {
	root, remote := t.TempDir(), t.TempDir()
	artifactWrite(t, root, "nested/output/old", "stale", 0o644)
	if err := os.MkdirAll(filepath.Join(remote, "nested", "output"), 0o700); err != nil {
		t.Fatal(err)
	}
	outputs := []string{"nested/output"}
	baseline, err := inspectArtifacts(root, outputs, true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), artifactFile)
	hash, _, err := collectArtifacts(remote, path, outputs)
	if err != nil {
		t.Fatal(err)
	}
	extracted := t.TempDir()
	if err := unpackArtifacts(resultRead(t, path), hash, outputs, extracted); err != nil {
		t.Fatal(err)
	}
	if err := publishArtifacts(t.TempDir(), root, extracted, baseline); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "nested", "output"))
	if err != nil || len(entries) != 0 {
		t.Fatal("empty output did not remove stale children")
	}
}

func TestArtifactDeclarationsAndCollectionRejectUnsafeInputs(t *testing.T) {
	for _, value := range []string{"null", "{}", "\"dist\"", "[1]"} {
		t.Setenv("SWITCHYARD_OUTPUTS", value)
		if _, err := declaredOutputs(nil); err == nil {
			t.Fatalf("accepted invalid output declaration: %s", value)
		}
	}
	t.Setenv("SWITCHYARD_OUTPUTS", `[]`)
	if _, err := declaredOutputs(nil); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{{"."}, {"../outside"}, {"/absolute"}, {"dist", "dist/file"}, {"dist", "dist"}, {".ssh"}, {"dist/*"}, {"dist/.env"}, {".switchyard-return-old"}} {
		if _, err := normalizeOutputs(paths); err == nil {
			t.Fatalf("accepted unsafe outputs: %q", paths)
		}
	}
	if err := validateOutputInputs([]string{"dist"}, []fileEntry{{Path: "dist/source.js"}}); err == nil {
		t.Fatal("accepted overlap with source inputs")
	}
	for _, scenario := range []string{"missing", "private", "symlink", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			switch scenario {
			case "private":
				artifactWrite(t, root, "dist/.env", "PRIVATE", 0o600)
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "dist")); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				artifactWrite(t, root, "dist/big", "", 0o600)
				if err := os.Truncate(filepath.Join(root, "dist", "big"), maxFileSize+1); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), artifactFile)
			if _, _, err := collectArtifacts(root, path, []string{"dist"}); err == nil {
				t.Fatal("unsafe/incomplete collection succeeded")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("incomplete collection published an archive")
			}
		})
	}
}

func TestArtifactRootsAreProtectedFromSourcePatches(t *testing.T) {
	for _, path := range []string{"output/file", "output/file/child", "output"} {
		t.Run(path, func(t *testing.T) {
			root, home := fixture(t), t.TempDir()
			captured, patch := resultFixture(t, root, home, func(checkout string) {
				artifactWrite(t, checkout, path, "unexpected source change\n", 0o644)
			})
			if err := returnWorkerEdits(home, root, captured, patch, "output/file"); err == nil {
				t.Fatal("source patch overwrote an artifact root or its ancestor")
			}
			if _, err := os.Stat(filepath.Join(root, "output")); !os.IsNotExist(err) {
				t.Fatal("rejected patch wrote to the workspace")
			}
		})
	}
}

func TestArtifactArchiveRejectsTamperingAndUnexpectedMembers(t *testing.T) {
	path, hash, _ := artifactBundleFixture(t)
	valid := resultRead(t, path)
	for _, scenario := range []string{"truncated", "digest", "file digest", "trailing"} {
		t.Run(scenario, func(t *testing.T) {
			data := append([]byte(nil), valid...)
			expected := hash
			switch scenario {
			case "truncated":
				data = data[:len(data)/2]
			case "digest":
				expected = strings.Repeat("0", 64)
			case "file digest":
				i := bytes.Index(data, []byte("\x00new output\xff"))
				if i < 0 {
					t.Fatal("missing fixture member")
				}
				data[i+1] ^= 1
				h := sha256.Sum256(data)
				expected = hex.EncodeToString(h[:])
			case "trailing":
				data = append(data, []byte("unexpected")...)
				h := sha256.Sum256(data)
				expected = hex.EncodeToString(h[:])
			}
			if err := unpackArtifacts(data, expected, []string{"dist", "report.txt"}, t.TempDir()); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
	for _, path := range []string{"../escape", "dist/../escape", "dist/.env", "other/file"} {
		var data bytes.Buffer
		w := tar.NewWriter(&data)
		m := artifactManifest{Version: 1, Outputs: []string{"dist"}, Entries: []artifactEntry{{fileEntry: fileEntry{Path: path, Mode: 0o755}, Kind: "directory"}}}
		metadata, _ := json.Marshal(m)
		if err := w.WriteHeader(&tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(metadata))}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(metadata); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(data.Bytes())
		if err := unpackArtifacts(data.Bytes(), hex.EncodeToString(h[:]), []string{"dist"}, t.TempDir()); err == nil {
			t.Fatalf("unsafe manifest accepted: %s", path)
		}
	}
}

func TestArtifactCollectionIsIndependentOfSourcePatch(t *testing.T) {
	root := t.TempDir()
	original, checkout := filepath.Join(root, "original"), filepath.Join(root, "checkout")
	for _, dir := range []string{original, checkout} {
		artifactWrite(t, dir, "README.md", "original\n", 0o644)
		artifactWrite(t, dir, ".gitignore", "dist/\n", 0o644)
	}
	artifactWrite(t, checkout, "README.md", "source edit\n", 0o644)
	artifactWrite(t, checkout, "dist/new.bin", "output\x00", 0o644)
	patch := filepath.Join(root, "changes.patch")
	if reason := collectPatch(original, checkout, patch, "dist"); reason != "" {
		t.Fatal(reason)
	}
	content := resultRead(t, patch)
	if bytes.Contains(content, []byte("dist/")) || !bytes.Contains(content, []byte("+source edit")) {
		t.Fatal("source patch mixed with artifacts")
	}
	if _, _, err := collectArtifacts(checkout, filepath.Join(root, artifactFile), []string{"dist"}); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactCleanupRequiresVerifiedDurableArchive(t *testing.T) {
	for _, scenario := range []string{"complete", "missing", "corrupt", "collection failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			for _, tree := range []string{"incoming", "worktree"} {
				if err := os.Mkdir(filepath.Join(dir, tree), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, file := range []string{"job.json", "changes.patch", "output.log"} {
				recoveryWrite(t, filepath.Join(dir, file), "", 0o600)
			}
			archive, hash, size := artifactBundleFixture(t)
			status := 0
			record := jobRecord{State: "finished", ExitStatus: &status, Outputs: []string{"dist", "report.txt"}, ArtifactHash: hash, ArtifactSize: size}
			if scenario != "missing" {
				recoveryWrite(t, filepath.Join(dir, artifactFile), string(resultRead(t, archive)), 0o600)
			}
			if scenario == "corrupt" {
				record.ArtifactHash = strings.Repeat("0", 64)
			}
			if scenario == "collection failure" {
				record.ArtifactError = "collection failed"
			}
			err := releaseJobTrees(dir, record)
			_, statErr := os.Stat(filepath.Join(dir, "worktree"))
			if scenario == "complete" {
				if err != nil || !os.IsNotExist(statErr) {
					t.Fatalf("complete result retained checkout: %v", err)
				}
			} else if statErr != nil {
				t.Fatal("incomplete artifacts lost their checkout")
			}
		})
	}
}
