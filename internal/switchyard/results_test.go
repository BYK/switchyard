package switchyard

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func resultFixture(t *testing.T, root, home string, edit func(string)) (capture, []byte) {
	t.Helper()
	captured, _, err := captureFiles(root, nil, home)
	if err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	original, checkout := filepath.Join(stage, "original"), filepath.Join(stage, "checkout")
	if err := copyTree(captured.Tree, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(original, manifestName)); err != nil {
		t.Fatal(err)
	}
	if err := copyTree(original, checkout); err != nil {
		t.Fatal(err)
	}
	edit(checkout)
	path := filepath.Join(stage, "result.patch")
	if reason := collectPatch(original, checkout, path); reason != "" {
		t.Fatal(reason)
	}
	patch, err := os.ReadFile(path)
	if err != nil || len(patch) == 0 {
		t.Fatalf("missing worker patch: %v", err)
	}
	return captured, patch
}

func resultRead(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestReturnWorkerEditsUsesCapturedWIPAndPreservesIndex(t *testing.T) {
	root, home := fixture(t), t.TempDir()
	// The Git index still says "original"; publication must use this dirty input.
	recoveryWrite(t, filepath.Join(root, "README.md"), "dirty input\r\n", 0o600)
	recoveryWrite(t, filepath.Join(root, ".gitattributes"), "* text eol=lf filter=fixture\n", 0o600)
	recoveryWrite(t, filepath.Join(root, "binary"), "\x00\x01before\xff", 0o600)
	recoveryWrite(t, filepath.Join(root, "remove.txt"), "delete me", 0o600)
	recoveryWrite(t, filepath.Join(root, "script"), "#!/bin/sh\n", 0o644)
	index := resultRead(t, filepath.Join(root, ".git", "index"))
	captured, patch := resultFixture(t, root, home, func(checkout string) {
		recoveryWrite(t, filepath.Join(checkout, "README.md"), "remote output\r\n", 0o644)
		recoveryWrite(t, filepath.Join(checkout, "binary"), "\x00\x02after\xfe", 0o644)
		recoveryWrite(t, filepath.Join(checkout, "space and 'quote'.txt"), "new output\r\n", 0o644)
		if err := os.Remove(filepath.Join(checkout, "remove.txt")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(checkout, "script"), 0o755); err != nil {
			t.Fatal(err)
		}
	})
	// Unrelated edits are permitted, even though they postdate capture.
	recoveryWrite(t, filepath.Join(root, "new.txt"), "concurrent unrelated edit", 0o600)
	// Inherited Git overrides must not redirect publication into the user's index.
	t.Setenv("GIT_INDEX_FILE", filepath.Join(root, ".git", "index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "filter.fixture.clean")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	if err := returnWorkerEdits(home, root, captured, patch); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{
		"README.md": "remote output\r\n", "binary": "\x00\x02after\xfe",
		"space and 'quote'.txt": "new output\r\n", "new.txt": "concurrent unrelated edit",
	} {
		if got := string(resultRead(t, filepath.Join(root, path))); got != expected {
			t.Fatalf("%q: got %q, want %q", path, got, expected)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "remove.txt")); !os.IsNotExist(err) {
		t.Fatal("deleted file was not returned")
	}
	if info, err := os.Stat(filepath.Join(root, "script")); err != nil || info.Mode()&0o111 == 0 {
		t.Fatal("executable bit was not returned")
	}
	if !bytes.Equal(index, resultRead(t, filepath.Join(root, ".git", "index"))) {
		t.Fatal("publication modified the user's Git index")
	}
	if _, err := verifyTree(captured.Tree); err != nil {
		t.Fatalf("publication modified the captured input: %v", err)
	}
}

func TestReturnWorkerEditsConflictsLeaveWholePatchUnapplied(t *testing.T) {
	for _, scenario := range []string{"edit", "delete", "mode", "new file", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root, home := fixture(t), t.TempDir()
			captured, patch := resultFixture(t, root, home, func(checkout string) {
				recoveryWrite(t, filepath.Join(checkout, "README.md"), "remote\n", 0o644)
				recoveryWrite(t, filepath.Join(checkout, "new.txt"), "remote untracked\n", 0o644)
				if err := os.Mkdir(filepath.Join(checkout, "output"), 0o700); err != nil {
					t.Fatal(err)
				}
				recoveryWrite(t, filepath.Join(checkout, "output", "new.txt"), "remote addition\n", 0o644)
			})
			switch scenario {
			case "edit":
				recoveryWrite(t, filepath.Join(root, "README.md"), "local\n", 0o600)
			case "delete":
				if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(filepath.Join(root, "README.md"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "new file":
				if err := os.Mkdir(filepath.Join(root, "output"), 0o700); err != nil {
					t.Fatal(err)
				}
				recoveryWrite(t, filepath.Join(root, "output", "new.txt"), "local addition\n", 0o600)
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(root, "output")); err != nil {
					t.Fatal(err)
				}
			}
			if err := returnWorkerEdits(home, root, captured, patch); !errors.Is(err, errResultConflict) {
				t.Fatalf("expected conflict, got %v", err)
			}
			if got := string(resultRead(t, filepath.Join(root, "new.txt"))); got != "new untracked file\n" {
				t.Fatal("conflict partially applied the patch")
			}
			if scenario == "edit" && string(resultRead(t, filepath.Join(root, "README.md"))) != "local\n" {
				t.Fatal("conflict overwrote the user's edit")
			}
		})
	}
}

func TestReturnWorkerEditsRejectsUnsafePatch(t *testing.T) {
	root, home := fixture(t), t.TempDir()
	captured, _, err := captureFiles(root, nil, home)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{".env", "nested/.env", ".git/config", "../escape"} {
		t.Run(path, func(t *testing.T) {
			patch := []byte("diff --git a/" + path + " b/" + path + "\nnew file mode 100644\n--- /dev/null\n+++ b/" + path + "\n@@ -0,0 +1 @@\n+unsafe\n")
			if err := returnWorkerEdits(home, root, captured, patch); err == nil {
				t.Fatal("unsafe path was accepted")
			}
		})
	}
	patch := []byte("diff --git a/link b/link\nnew file mode 120000\n--- /dev/null\n+++ b/link\n@@ -0,0 +1 @@\n+/tmp\n")
	if err := returnWorkerEdits(home, root, captured, patch); err == nil {
		t.Fatal("symlink was accepted")
	}
	if _, err := os.Lstat(filepath.Join(root, "link")); !os.IsNotExist(err) {
		t.Fatal("symlink was written")
	}
	if string(resultRead(t, filepath.Join(root, ".env"))) != "PRIVATE_SENTINEL_DO_NOT_COPY\n" {
		t.Fatal("private file was changed")
	}
}

func TestReturnWorkerEditsSerializesCompetingJobs(t *testing.T) {
	root, home := fixture(t), t.TempDir()
	var inputs [2]capture
	var patches [2][]byte
	for i, content := range []string{"first\n", "second\n"} {
		inputs[i], patches[i] = resultFixture(t, root, home, func(checkout string) {
			recoveryWrite(t, filepath.Join(checkout, "README.md"), content, 0o644)
		})
	}
	start, done := make(chan struct{}), make(chan error, 2)
	for i := range inputs {
		go func(i int) {
			<-start
			done <- returnWorkerEdits(home, root, inputs[i], patches[i])
		}(i)
	}
	close(start)
	applied, conflicts := 0, 0
	for range inputs {
		err := <-done
		if err == nil {
			applied++
		} else if errors.Is(err, errResultConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if applied != 1 || conflicts != 1 {
		t.Fatalf("competing jobs: applied=%d conflicts=%d", applied, conflicts)
	}
}

func TestEditReturnCLI(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	executable := binary(t)
	for _, scenario := range []struct {
		action, mode, state string
		conflict            bool
	}{
		{"proxy", "", "applied", false},
		{"run", "", "review", false},
		{"proxy", "patch", "review", false},
		{"run", "apply", "applied", false},
		{"proxy", "", "conflict", true},
		{"hook", "", "applied", false},
		{"hook", "patch", "review", false},
	} {
		t.Run(scenario.action+"/"+scenario.state+"/"+scenario.mode, func(t *testing.T) {
			root, home := fixture(t), t.TempDir()
			t.Setenv("SWITCHYARD_HOME", home)
			t.Setenv("SWITCHYARD_RETURN_MODE", scenario.mode)
			// Exercise the automatic policy without relying on host CPU pressure.
			t.Setenv("SWITCHYARD_TRACE_HANDOFF", "1")
			t.Setenv("SWITCHYARD_BACKEND", "auto")
			worker := filepath.Join(t.TempDir(), "worker")
			_, patch := resultFixture(t, root, home, func(checkout string) {
				recoveryWrite(t, filepath.Join(checkout, "README.md"), "remote\n", 0o644)
			})
			patchPath := filepath.Join(t.TempDir(), "result.patch")
			recoveryWrite(t, patchPath, string(patch), 0o600)
			adapter := resultAdapter(t, executable, patchPath)
			if err := saveBackend(home, backendConfig{Name: "sandbox", Kind: "plugin", Root: worker, Command: adapter}); err != nil {
				t.Fatal(err)
			}
			if err := initWorker(worker); err != nil {
				t.Fatal(err)
			}
			// The provider gives the fixture a deterministic concurrent local edit.
			// The absolute paths are test coordination, not a remote-provider feature.
			t.Setenv("RESULT_SOURCE", root)
			count := filepath.Join(t.TempDir(), "executions")
			t.Setenv("RESULT_COUNT", count)
			script := `printf x >> "$RESULT_COUNT"; printf 'remote\n' > README.md; printf 'output\n'; printf 'error\n' >&2; `
			if scenario.conflict {
				script += `printf 'local\n' > "$RESULT_SOURCE/README.md"; `
			}
			script += "exit 37"
			args := []string{scenario.action, "--backend", "sandbox", "--repo", root, "--", "sh", "-c", script}
			if scenario.action == "hook" {
				t.Setenv("SWITCHYARD_TRACE_HANDOFF", "")
				t.Setenv("SWITCHYARD_BACKEND", "sandbox")
				args = []string{"hook", "--", "sh", "-c", script}
			}
			stdout, stderr, status := recoveryCommand(t, root, executable, args...)
			if status != 37 || !strings.Contains(stdout, "output\n") {
				t.Fatalf("command result: %d %q %q", status, stdout, stderr)
			}
			if scenario.action != "run" && stdout != "output\n" {
				t.Fatalf("proxy output changed: %q", stdout)
			}
			if scenario.conflict {
				if !strings.Contains(stderr, "edit return conflict") {
					t.Fatalf("missing conflict: %q", stderr)
				}
			} else if stderr != "error\n" {
				t.Fatalf("command stderr changed: %q", stderr)
			}
			if string(resultRead(t, count)) != "x" {
				t.Fatal("command was replayed")
			}
			expected := "original\n"
			if scenario.state == "applied" {
				expected = "remote\n"
			}
			if scenario.conflict {
				expected = "local\n"
			}
			if string(resultRead(t, filepath.Join(root, "README.md"))) != expected {
				t.Fatal("unexpected local result")
			}
			patches, _ := filepath.Glob(filepath.Join(home, "runs", "*.patch"))
			if len(patches) != 1 || !bytes.Contains(resultRead(t, patches[0]), []byte("+remote")) {
				t.Fatal("patch was not retained")
			}
			job := strings.TrimSuffix(filepath.Base(patches[0]), ".patch")
			record, err := loadLocalJob(home, job)
			if err != nil || record.EditReturn != scenario.state {
				t.Fatalf("return status: %+v %v", record, err)
			}
			out, _, code := recoveryCommand(t, root, executable, "jobs")
			if code != 0 || !strings.Contains(out, "edits="+scenario.state) {
				t.Fatalf("jobs lost return status: %s", out)
			}
			out, _, code = recoveryCommand(t, root, executable, "status", job)
			if code != 0 || !strings.Contains(out, "Worker edits: "+scenario.state) {
				t.Fatalf("status lost return status: %s", out)
			}
		})
	}
	t.Run("invalid mode never executes", func(t *testing.T) {
		t.Setenv("SWITCHYARD_HOME", t.TempDir())
		t.Setenv("SWITCHYARD_RETURN_MODE", "typo")
		stdout, stderr, status := recoveryCommand(t, t.TempDir(), executable, "proxy", "--backend", "auto", "--", "sh", "-c", "printf executed")
		if status != 2 || stdout != "" || !strings.Contains(stderr, "SWITCHYARD_RETURN_MODE") {
			t.Fatalf("invalid mode executed: %d %q %q", status, stdout, stderr)
		}
	})
}

// This provider executes the requested command in a separate checkout and
// publishes a known patch/status through the real worker protocol. It isolates
// the client return contract from Linux process-group supervision (which is
// exercised by the worker tests), so it also works in restricted PID namespaces.
func resultAdapter(t *testing.T, executable, patchPath string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider")
	recoveryWrite(t, path, `#!/usr/bin/env python3
import base64, json, os, pathlib, shutil, subprocess, sys
if sys.argv[1:] == ['--version']:
    print('switchyard-provider-v1')
    sys.exit(0)
root = pathlib.Path(sys.argv[3])
request = sys.stdin.buffer.readline() if sys.argv[4:] == ['wire'] else b''
payload = json.loads(request) if request else {}
args = payload.get('args', sys.argv[4:])
if sys.argv[1] == 'sync':
    args = sys.argv[4:]
    source = args[args.index('--source')+1]
    target = root/'projects'/args[args.index('--project')+1]/'jobs'/args[args.index('--job')+1]/'incoming'/'files'
    shutil.copytree(source, target, dirs_exist_ok=True)
    sys.exit(0)
if args[0] == 'execute':
    job, project = args[args.index('--job')+1], args[args.index('--project')+1]
    directory = root/'projects'/project/'jobs'/job
    checkout = directory/'worktree'
    shutil.copytree(directory/'incoming'/'files', checkout)
    manifest = json.loads((checkout/'.switchyard-manifest.json').read_text())
    assert manifest['snapshot'] == args[args.index('--snapshot')+1]
    assert (checkout/'README.md').read_bytes() == b'original\n'
    environment = dict(base64.b64decode(entry).decode().split('=', 1) for entry in payload['environment'])
    completed = subprocess.run(args[args.index('--')+1:], cwd=checkout/args[args.index('--cwd')+1], env=environment, stdin=sys.stdin.buffer)
    assert (checkout/'README.md').read_bytes() == b'remote\n'
    shutil.copyfile(`+strconv.Quote(patchPath)+`, directory/'changes.patch')
    (directory/'changes.patch').chmod(0o600)
    record = {'id':job, 'project':project, 'snapshot':manifest['snapshot'], 'state':'finished', 'exit_status':completed.returncode, 'has_edits':True}
    (directory/'job.json').write_text(json.dumps(record))
    (directory/'job.json').chmod(0o600)
    sys.exit(completed.returncode)
binary = `+strconv.Quote(executable)+`
if request:
    sys.exit(subprocess.run([binary]+sys.argv[1:], input=request+sys.stdin.buffer.read()).returncode)
os.execv(binary, [binary]+sys.argv[1:])
`, 0o755)
	return path
}
