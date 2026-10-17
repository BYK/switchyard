package switchyard

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func patchFixtureInputs(tb testing.TB, root string) []fileEntry {
	tb.Helper()
	var entries []fileEntry
	err := filepath.WalkDir(root, func(path string, item os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if excludedPath(rel) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if item.IsDir() {
			return nil
		}
		hash, info, err := digestFile(root, rel)
		if err != nil {
			return err
		}
		entries = append(entries, fileEntry{Path: rel, Hash: hash, Size: info.Size(), Mode: normalizedMode(info.Mode())})
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	return entries
}

func TestPatchMatchesFullTreeCollector(t *testing.T) {
	for _, scenario := range []string{"unchanged", "edits", "ignored", "new-file", "delete-all", "file-directory", "directory-file"} {
		t.Run(scenario, func(t *testing.T) {
			job := t.TempDir()
			original, checkout := filepath.Join(job, "original"), filepath.Join(job, "checkout")
			for path, content := range map[string]string{
				"text": "before\r\n", "binary": "\x00\xffbefore", "script": "#!/bin/sh\n",
				"remove": "remove me", "kept": "untouched", "ignored/captured": "explicit input",
				".gitignore": "ignored/\n*.generated\n", ".gitattributes": "* text eol=lf filter=fixture\n",
				"file": "was a file", "directory/child": "was a directory",
			} {
				artifactWrite(t, original, path, content, 0o644)
			}
			inputs := patchFixtureInputs(t, original)
			if err := copyTree(original, checkout); err != nil {
				t.Fatal(err)
			}
			expected := filepath.Join(job, "expected")
			if err := copyTree(original, expected); err != nil {
				t.Fatal(err)
			}
			mutate := func(root string) {
				switch scenario {
				case "new-file":
					artifactWrite(t, root, "new-source", "new file", 0o644)
				case "delete-all":
					if err := os.RemoveAll(root); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(root, 0o700); err != nil {
						t.Fatal(err)
					}
				case "edits":
					for path, content := range map[string]string{
						"text": "after!\r\n", "binary": "\x00\xfeafter!", "ignored/captured": "still tracked",
						"space and 'quote'": "new", ":(glob)literal*": "literal path", "café": "unicode",
					} {
						artifactWrite(t, root, path, content, 0o644)
					}
					if err := os.Chmod(filepath.Join(root, "script"), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(filepath.Join(root, "remove")); err != nil {
						t.Fatal(err)
					}
				case "ignored":
					artifactWrite(t, root, ".gitignore", "ignored/\n*.generated\nnew-rule\n", 0o644)
					artifactWrite(t, root, "ignored/captured", "still tracked", 0o644)
					artifactWrite(t, root, "included", "new source", 0o644)
				case "file-directory":
					if err := os.Remove(filepath.Join(root, "file")); err != nil {
						t.Fatal(err)
					}
					artifactWrite(t, root, "file/child", "now a directory", 0o644)
				case "directory-file":
					if err := os.RemoveAll(filepath.Join(root, "directory")); err != nil {
						t.Fatal(err)
					}
					artifactWrite(t, root, "directory", "now a file", 0o644)
				}
			}
			mutate(checkout)
			mutate(expected)
			if scenario == "ignored" {
				for _, path := range []string{"ignored/new", "new-rule", "new.generated", "node_modules/cache", ".env", "dist/output"} {
					artifactWrite(t, checkout, path, "must not return", 0o644)
				}
			}
			// Content changes with restored timestamps must still be detected.
			if scenario == "edits" {
				stamp := time.Unix(1_700_000_000, 0)
				for _, root := range []string{original, checkout} {
					if err := os.Chtimes(filepath.Join(root, "text"), stamp, stamp); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Neither inherited configuration nor repository attributes may run a
			// content filter while preparing the patch.
			t.Setenv("GIT_CONFIG_COUNT", "2")
			t.Setenv("GIT_CONFIG_KEY_0", "filter.fixture.clean")
			t.Setenv("GIT_CONFIG_VALUE_0", "false")
			t.Setenv("GIT_CONFIG_KEY_1", "filter.fixture.required")
			t.Setenv("GIT_CONFIG_VALUE_1", "true")
			for _, method := range []struct {
				name    string
				collect func(string, string, string, []fileEntry, ...string) string
			}{{"full-tree", collectPatchFullTreeBaseline}, {"selective", collectPatch}} {
				patch := filepath.Join(job, method.name+".patch")
				if reason := method.collect(original, checkout, patch, inputs, "dist"); reason != "" {
					t.Fatalf("%s: %s", method.name, reason)
				}
				result := filepath.Join(job, method.name+"-result")
				if err := copyTree(original, result); err != nil {
					t.Fatal(err)
				}
				data := resultRead(t, patch)
				if scenario == "unchanged" {
					if len(data) != 0 {
						t.Fatalf("%s: unchanged tree produced a patch", method.name)
					}
				} else {
					cmd := exec.Command("git", "apply", "--binary", "--", patch)
					cmd.Dir, cmd.Env = result, patchEnvironment(filepath.Join(job, "unused-index"))
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("%s apply: %v %s", method.name, err, out)
					}
				}
				if !reflect.DeepEqual(patchFixtureInputs(t, result), patchFixtureInputs(t, expected)) {
					t.Fatalf("%s patch did not restore expected contents and modes", method.name)
				}
				for _, private := range []string{".env", "dist/output", "node_modules/cache"} {
					if _, err := os.Lstat(filepath.Join(result, private)); !os.IsNotExist(err) {
						t.Fatalf("%s returned excluded path %s", method.name, private)
					}
				}
			}
			assertNoPatchStage(t, job)
		})
	}
}

func assertNoPatchStage(t *testing.T, job string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(job, ".patch-collect-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("temporary patch data remains: %v %v", paths, err)
	}
}

func TestPatchRejectsChangedBaselineAndCleansUp(t *testing.T) {
	for _, scenario := range []string{"changed-baseline", "missing-baseline", "symlink", "oversize", "invalid-manifest", "duplicate-manifest", "artifact-overlap"} {
		t.Run(scenario, func(t *testing.T) {
			job := t.TempDir()
			original, checkout := filepath.Join(job, "original"), filepath.Join(job, "checkout")
			artifactWrite(t, original, "text", "before", 0o644)
			inputs := patchFixtureInputs(t, original)
			artifactWrite(t, checkout, "text", "after!", 0o644)
			var outputs []string
			switch scenario {
			case "changed-baseline":
				artifactWrite(t, original, "text", "broken", 0o644)
			case "missing-baseline":
				if err := os.Remove(filepath.Join(original, "text")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(original, filepath.Join(checkout, "link")); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.Truncate(filepath.Join(checkout, "text"), maxFileSize+1); err != nil {
					t.Fatal(err)
				}
			case "invalid-manifest":
				inputs[0].Path = "../escape"
			case "duplicate-manifest":
				inputs = append(inputs, inputs[0])
			case "artifact-overlap":
				outputs = []string{"text"}
			}
			patch := filepath.Join(job, "result.patch")
			if reason := collectPatch(original, checkout, patch, inputs, outputs...); reason == "" {
				t.Fatal("unsafe or incomplete input accepted")
			}
			if _, err := os.Stat(patch); !os.IsNotExist(err) {
				t.Fatal("failed collection left a partial patch")
			}
			if _, err := os.Stat(checkout); err != nil {
				t.Fatal("failed collection removed recoverable checkout")
			}
			assertNoPatchStage(t, job)
		})
	}
}

func TestPatchFrozenBytesMustMatchScan(t *testing.T) {
	for _, change := range []string{"content", "mode"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			artifactWrite(t, root, "text", "before", 0o644)
			inputs := patchFixtureInputs(t, root)
			if change == "content" {
				artifactWrite(t, root, "text", "after!", 0o644)
			} else if err := os.Chmod(filepath.Join(root, "text"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := freezePatchFiles(root, filepath.Join(t.TempDir(), "frozen"), inputs); err == nil {
				t.Fatal("changed input accepted after scan")
			}
		})
	}
}

func TestPatchExportFailureRemovesPartialOutput(t *testing.T) {
	job := t.TempDir()
	patch := filepath.Join(job, "result.patch")
	// A missing baseline tree makes Git fail after the output file is opened.
	if reason := exportPatch(filepath.Join(job, "index"), filepath.Join(job, "missing.git"), job, patch, strings.Repeat("0", 40)); reason == "" {
		t.Fatal("failed Git export accepted")
	}
	if _, err := os.Stat(patch); !os.IsNotExist(err) {
		t.Fatal("failed export left partial output")
	}
	// Existing destinations must never be replaced, including the empty path.
	artifactWrite(t, job, "result.patch", "preserve", 0o600)
	original, checkout := filepath.Join(job, "original"), filepath.Join(job, "checkout")
	artifactWrite(t, original, "text", "same", 0o644)
	artifactWrite(t, checkout, "text", "same", 0o644)
	if reason := collectPatch(original, checkout, patch, patchFixtureInputs(t, original)); reason == "" {
		t.Fatal("existing patch overwritten")
	}
	if !bytes.Equal(resultRead(t, patch), []byte("preserve")) {
		t.Fatal("existing patch changed")
	}
}
