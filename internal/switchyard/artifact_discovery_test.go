package switchyard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestArtifactAutomaticDiscoveryAndReturn(t *testing.T) {
	root, home := fixture(t), t.TempDir()
	artifactWrite(t, root, ".gitignore", "dist/\nreport.txt\n*.generated\n.env\n", 0o644)
	artifactWrite(t, root, "packages/web/main.go", "source", 0o644)
	artifactWrite(t, root, "src/main.go", "source", 0o644)
	artifactWrite(t, root, "dist/old", "obsolete", 0o644)
	artifactWrite(t, root, "report.txt", "old report", 0o644)
	captured, _, err := captureFiles(root, nil, home)
	if err != nil {
		t.Fatal(err)
	}
	before := captureAutomaticArtifacts(root, captured.Manifest.Files)
	if before.err != nil {
		t.Fatal(before.err)
	}
	remote := filepath.Join(t.TempDir(), "checkout")
	if err := copyTree(captured.Tree, remote); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(remote, manifestName)); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{
		"dist/new": "build", "report.txt": "report", "packages/web/dist/bundle.js": "bundle",
		"src/file.generated": "generated", "src/added.go": "new source", ".env": "private",
		"node_modules/dependency/index.js": "dependency", ".cache/build": "cache",
	} {
		artifactWrite(t, remote, path, value, 0o644)
	}
	outputs, err := discoverArtifacts(remote, captured.Manifest.Files)
	want := []string{"dist", "packages/web/dist", "report.txt", "src/file.generated"}
	if err != nil || !reflect.DeepEqual(outputs, want) {
		t.Fatalf("discovered %q; want %q: %v", outputs, want, err)
	}
	baseline, err := before.selectRoots(outputs)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), artifactFile)
	hash, _, err := collectArtifacts(remote, archive, outputs)
	if err != nil {
		t.Fatal(err)
	}
	extracted := t.TempDir()
	if err := unpackArtifacts(resultRead(t, archive), hash, outputs, extracted); err != nil {
		t.Fatal(err)
	}
	if err := publishArtifacts(home, root, extracted, baseline); err != nil {
		t.Fatal(err)
	}
	if string(resultRead(t, filepath.Join(root, "dist", "new"))) != "build" {
		t.Fatal("automatically discovered build did not return")
	}
	if _, err := os.Stat(filepath.Join(root, "dist", "old")); !os.IsNotExist(err) {
		t.Fatal("automatic directory replacement retained a stale output")
	}
	if string(resultRead(t, filepath.Join(root, "packages", "web", "main.go"))) != "source" {
		t.Fatal("nested output overwrote source")
	}
	if _, err := os.Stat(filepath.Join(root, "src", "added.go")); !os.IsNotExist(err) {
		t.Fatal("new source was misclassified as an artifact")
	}
	if err := os.Remove(filepath.Join(captured.Tree, manifestName)); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(t.TempDir(), "changes.patch")
	if reason := collectPatch(captured.Tree, remote, patch, outputs...); reason != "" {
		t.Fatal(reason)
	}
	data := resultRead(t, patch)
	if !bytes.Contains(data, []byte("src/added.go")) || bytes.Contains(data, []byte("dist/new")) || bytes.Contains(data, []byte("private")) {
		t.Fatal("automatic discovery mixed artifacts or private files into source edits")
	}
}

func TestArtifactAutomaticDiscoveryPreservesExplicitInputsAndSupportsManyFiles(t *testing.T) {
	root := t.TempDir()
	artifactWrite(t, root, ".gitignore", "dist/\n*.generated\n", 0o644)
	artifactWrite(t, root, "dist/input", "explicit input", 0o644)
	artifactWrite(t, root, "dist/result", "output", 0o644)
	inputs := []fileEntry{{Path: ".gitignore"}, {Path: "dist/input"}}
	for i := 0; i < 40; i++ {
		artifactWrite(t, root, fmt.Sprintf("file-%02d.generated", i), "output", 0o644)
	}
	outputs, err := discoverArtifacts(root, inputs)
	if err != nil || len(outputs) != 41 || outputs[0] != "dist/result" {
		t.Fatalf("discovery replaced an input ancestor or limited automatic roots to 32: %q %v", outputs, err)
	}
	if err := validateOutputInputs(outputs, inputs); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactAutomaticBaselineProtectsPreviouslyUnknownRoots(t *testing.T) {
	for _, change := range []string{"edit", "create", "private removed", "symlink removed", "oversized"} {
		t.Run(change, func(t *testing.T) {
			root := t.TempDir()
			if change != "create" {
				artifactWrite(t, root, "dist/file", "old", 0o644)
			}
			switch change {
			case "private removed":
				artifactWrite(t, root, "dist/.env", "private", 0o600)
			case "symlink removed":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "dist", "link")); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if err := os.Truncate(filepath.Join(root, "dist", "file"), maxArtifactBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			before := captureAutomaticArtifacts(root, nil)
			switch change {
			case "edit", "create":
				artifactWrite(t, root, "dist/file", "local change", 0o644)
			case "private removed":
				if err := os.Remove(filepath.Join(root, "dist", ".env")); err != nil {
					t.Fatal(err)
				}
			case "symlink removed":
				if err := os.Remove(filepath.Join(root, "dist", "link")); err != nil {
					t.Fatal(err)
				}
			}
			baseline, err := before.selectRoots([]string{"dist"})
			if err == nil {
				remote := t.TempDir()
				artifactWrite(t, remote, "dist/remote", "remote", 0o644)
				err = publishArtifacts(t.TempDir(), root, remote, baseline)
			}
			if !errors.Is(err, errArtifactConflict) {
				t.Fatalf("unsafe automatic publication was not blocked: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "dist", "remote")); !os.IsNotExist(err) {
				t.Fatal("conflicted automatic return wrote output")
			}
		})
	}
}

func TestArtifactAutomaticBaselineDoesNotBlockUnrelatedOutputs(t *testing.T) {
	root := t.TempDir()
	artifactWrite(t, root, "large/ignored", "", 0o600)
	if err := os.Truncate(filepath.Join(root, "large", "ignored"), maxArtifactBytes+1); err != nil {
		t.Fatal(err)
	}
	artifactWrite(t, root, "node_modules/private/.env", "private dependency state", 0o600)
	artifactWrite(t, root, "dist/old", "old", 0o644)
	before := captureAutomaticArtifacts(root, nil)
	if _, err := before.selectRoots([]string{"dist"}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range before.entries {
		if excludedPath(entry.Path) || entry.Size > maxArtifactBytes {
			t.Fatal("baseline read private, dependency or oversized content")
		}
	}
}
