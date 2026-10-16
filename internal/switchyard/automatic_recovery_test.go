package switchyard

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAutomaticRecoveryPreservesCommandAndNeverDuplicatesExecution(t *testing.T) {
	for _, scenario := range []struct {
		name, mode, output string
		syncs              int
		native             bool
	}{
		{"prepare failure falls back", "prepare-fails", "native", 0, true},
		{"setup failure falls back", "setup-fails", "native", 1, true},
		{"planning failure falls back quietly", "plan-fails", "native", 1, true},
		{"transfer failure retries then falls back", "sync-fails", "native", 2, true},
		{"transient transfer failure recovers", "sync-once", "remote", 2, false},
		{"lost execution response reconciles status", "execute-lost", "remote", 1, false},
		{"unknown execution outcome never reruns", "execute-status-lost", "remote", 1, false},
		{"invalid status preserves known exit", "invalid-status", "remote", 1, false},
		{"lost status does not rerun command", "status-fails", "remote", 1, false},
		{"patch failure retains command exit", "patch-fails", "remote", 1, false},
		{"patch save failure retains command exit", "patch-save-fails", "remote", 1, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			root, home, repo, adapter, executable := automaticRecoveryFixture(t)
			cfg := backendConfig{Name: "fixture", Kind: "plugin", Root: filepath.Join(root, "worker"), Command: adapter}
			if err := saveBackend(home, cfg); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RECOVERY_MODE", scenario.mode)
			// The traced automatic entrypoint already supplies its selected backend.
			// This exercises the same automatic execution policy without depending on
			// host pressure or changing the host's /proc mount.
			t.Setenv("SWITCHYARD_TRACE_HANDOFF", "1")
			t.Setenv("SWITCHYARD_BACKEND", "auto")
			out, stderr, status := recoveryCommand(t, root, executable, "proxy", "--repo", repo, "--backend", "fixture", "--", "pnpm", "test")
			expected := 37
			if scenario.mode == "execute-status-lost" {
				expected = 255
			}
			if status != expected || out != scenario.output+" stdout\n" || stderr != scenario.output+" stderr\n" {
				t.Fatalf("command output or exit changed: exit=%d stdout=%q stderr=%q", status, out, stderr)
			}
			count, _ := os.ReadFile(filepath.Join(root, "sync-count"))
			if len(count) != scenario.syncs {
				t.Fatalf("sync attempts = %d; want %d", len(count), scenario.syncs)
			}
			native, _ := os.ReadFile(filepath.Join(root, "native-count"))
			remote, _ := os.ReadFile(filepath.Join(root, "remote-count"))
			if scenario.native && (len(native) != 1 || len(remote) != 0) || !scenario.native && (len(native) != 0 || len(remote) != 1) {
				t.Fatalf("command duplicated or lost: native=%d remote=%d", len(native), len(remote))
			}
			if scenario.mode == "sync-fails" {
				files, _ := filepath.Glob(filepath.Join(root, "worker", "projects", "*", "jobs", "*", "incoming"))
				if len(files) != 0 {
					t.Fatal("failed transfers retained partial snapshots")
				}
				if load, err := workerLoad(filepath.Join(root, "worker")); err != nil || load != 0 {
					t.Fatalf("discarded preparation prevents idle stop: load=%d err=%v", load, err)
				}
			}
			if !scenario.native {
				jobs, _ := filepath.Glob(filepath.Join(root, "worker", "projects", "*", "jobs", "*"))
				if len(jobs) != 1 {
					t.Fatalf("expected one completed job, got %d", len(jobs))
				}
				for _, tree := range []string{"incoming", "worktree"} {
					if _, err := os.Stat(filepath.Join(jobs[0], tree)); !os.IsNotExist(err) {
						t.Fatalf("completed job retained %s: %v", tree, err)
					}
				}
				for _, result := range []string{"job.json", "changes.patch", "output.log"} {
					if _, err := os.Stat(filepath.Join(jobs[0], result)); err != nil {
						t.Fatalf("cleanup lost durable result %s: %v", result, err)
					}
				}
			}
			captures, err := os.ReadDir(filepath.Join(home, "snapshots"))
			if err == nil && len(captures) != 0 {
				t.Fatalf("fallback leaked %d captured snapshots", len(captures))
			}
			logs, err := os.ReadFile(filepath.Join(home, "logs", "events.jsonl"))
			if err != nil || len(logs) == 0 {
				t.Fatalf("missing private diagnostics: %v", err)
			}
			for _, secret := range []string{"DO_NOT_LOG", repo, "pnpm test", "environment-secret"} {
				if strings.Contains(string(logs), secret) {
					t.Fatalf("diagnostics disclosed private input %q", secret)
				}
			}
			for _, line := range strings.Split(strings.TrimSpace(string(logs)), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("invalid JSON diagnostic: %q", line)
				}
			}
		})
	}
}

func TestExplicitTransferFailureDoesNotSilentlyRunLocally(t *testing.T) {
	root, home, repo, adapter, executable := automaticRecoveryFixture(t)
	if err := saveBackend(home, backendConfig{Name: "fixture", Kind: "plugin", Root: filepath.Join(root, "worker"), Command: adapter}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECOVERY_MODE", "sync-fails")
	out, stderr, status := recoveryCommand(t, root, executable, "proxy", "--repo", repo, "--backend", "fixture", "--", "pnpm", "test")
	if status == 0 || out != "" || !strings.Contains(stderr, "snapshot") {
		t.Fatalf("explicit failure was hidden: exit=%d stdout=%q stderr=%q", status, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "native-count")); !os.IsNotExist(err) {
		t.Fatal("explicit remote request ran locally")
	}
}

func TestExplicitResultFailuresPreserveCompletedExit(t *testing.T) {
	for _, mode := range []string{"patch-fails", "patch-save-fails", "status-fails", "invalid-status", "execute-lost"} {
		for _, expected := range []int{0, 37} {
			t.Run(mode+"/exit-"+strconv.Itoa(expected), func(t *testing.T) {
				root, home, repo, adapter, executable := automaticRecoveryFixture(t)
				if err := saveBackend(home, backendConfig{Name: "fixture", Kind: "plugin", Root: filepath.Join(root, "worker"), Command: adapter}); err != nil {
					t.Fatal(err)
				}
				t.Setenv("RECOVERY_MODE", mode)
				t.Setenv("RECOVERY_EXIT", strconv.Itoa(expected))
				out, stderr, status := recoveryCommand(t, root, executable, "proxy", "--repo", repo, "--backend", "fixture", "--", "pnpm", "test")
				if status != expected || !strings.HasSuffix(out, "remote stdout\n") || !strings.Contains(stderr, "remote stderr\n") {
					t.Fatalf("completed command changed: exit=%d stdout=%q stderr=%q", status, out, stderr)
				}
				remote, _ := os.ReadFile(filepath.Join(root, "remote-count"))
				if len(remote) != 1 {
					t.Fatalf("remote executions = %d; want 1", len(remote))
				}
				if _, err := os.Stat(filepath.Join(root, "native-count")); !os.IsNotExist(err) {
					t.Fatal("explicit result recovery reran the command locally")
				}
			})
		}
	}
}

func automaticRecoveryFixture(t *testing.T) (string, string, string, string, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	executable := binary(t)
	root := t.TempDir()
	home := filepath.Join(root, "state")
	repo := filepath.Join(root, "source")
	bin := filepath.Join(root, "bin")
	for _, directory := range []string{repo, bin} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("SWITCHYARD_HOME", home)
	t.Setenv("SWITCHYARD_AWS_HOME", filepath.Join(root, "aws"))
	t.Setenv("SWITCHYARD_TRACE_HANDOFF", "")
	t.Setenv("SWITCHYARD_BACKEND", "")
	t.Setenv("RECOVERY_ROOT", root)
	t.Setenv("RECOVERY_SOURCE", repo)
	t.Setenv("RECOVERY_EXIT", "37")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	recoveryCommand(t, repo, "git", "init", "-q")
	recoveryCommand(t, repo, "git", "config", "user.email", "fixture@example.invalid")
	recoveryCommand(t, repo, "git", "config", "user.name", "Fixture")
	recoveryWrite(t, filepath.Join(repo, "package.json"), `{"packageManager":"pnpm@10.28.0","scripts":{"test":"fixture"}}`, 0o644)
	recoveryWrite(t, filepath.Join(repo, "pnpm-lock.yaml"), "lockfileVersion: '9.0'\n", 0o644)
	recoveryCommand(t, repo, "git", "add", ".")
	recoveryCommand(t, repo, "git", "commit", "-qm", "fixture")
	recoveryWrite(t, filepath.Join(bin, "pnpm"), `#!/bin/sh
case "$1" in
  --version) printf '10.28.0\n'; exit 0 ;;
  install)
    printf 'dependency setup noise\n'
    printf 'dependency setup error noise\n' >&2
    if [ "$RECOVERY_MODE" = setup-fails ]; then exit 11; fi
    mkdir -p node_modules; exit 0 ;;
esac
if [ "$PWD" = "$RECOVERY_SOURCE" ]; then kind=native; else kind=remote; fi
printf x >> "$RECOVERY_ROOT/$kind-count"
if [ "$RECOVERY_MODE" = patch-fails ] || [ "$RECOVERY_MODE" = patch-save-fails ]; then printf changed > worker-note.txt; fi
printf '%s stdout\n' "$kind"
printf '%s stderr\n' "$kind" >&2
exit "${RECOVERY_EXIT:-37}"
`, 0o755)
	adapter := filepath.Join(root, "adapter")
	recoveryWrite(t, adapter, `#!/usr/bin/python3
import base64, json, os, subprocess, sys
if sys.argv[1:] == ['--version']:
    print('switchyard-provider-v1')
    sys.exit(0)
base = os.environ['RECOVERY_ROOT']
root = sys.argv[3]
wired = sys.argv[4:] == ['wire']
request = sys.stdin.buffer.readline() if wired else b''
args = json.loads(request)['args'] if wired else sys.argv[4:]
if sys.argv[1] == 'sync':
    args = ['sync', sys.argv[sys.argv.index('--source')+1], sys.argv[sys.argv.index('--project')+1], sys.argv[sys.argv.index('--job')+1]]
action = args[0]
mode = os.environ['RECOVERY_MODE']
if action == 'prepare' and mode == 'prepare-fails':
    sys.stderr.write('DO_NOT_LOG environment-secret\n')
    sys.exit(11)
if action == 'sync':
    count = os.path.join(base, 'sync-count')
    with open(count, 'ab') as file: file.write(b'x')
    if mode == 'sync-fails' or mode == 'sync-once' and os.path.getsize(count) == 1:
        sys.stderr.write('DO_NOT_LOG environment-secret\n')
        sys.exit(11)
    source, project, job = args[1:]
    os.execv('/usr/bin/rsync', ['rsync', '-a', '--checksum', '--delete', '--', source+'/', root+'/projects/'+project+'/jobs/'+job+'/incoming/files/'])
if action == 'status' and mode in ['status-fails', 'execute-status-lost'] or action == 'patch' and mode == 'patch-fails':
    sys.exit(11)
if action == 'patch' and mode == 'patch-save-fails':
    os.mkdir(os.path.join(base, 'state', 'runs', args[args.index('--job')+1]+'.patch'), 0o700)
binary = `+strconv.Quote(executable)+`
if not wired and action == 'status' and mode == 'invalid-status':
    completed = subprocess.run([binary, 'worker', '--root', root]+args, stdout=subprocess.PIPE)
    payload = json.loads(completed.stdout)
    payload['exit_status'] = 256
    print(json.dumps(payload))
    sys.exit(0)
if wired:
    if action == 'status' and mode == 'invalid-status':
        completed = subprocess.run([binary, 'worker', '--root', root, 'wire'], input=request+sys.stdin.buffer.read(), stdout=subprocess.PIPE)
        payload = json.loads(completed.stdout)
        payload['exit_status'] = 256
        print(json.dumps(payload))
        sys.exit(0)
    if action == 'execute' and mode == 'plan-fails':
        payload = json.loads(request)
        payload['environment'] = [base64.b64encode(b'PATH=/missing').decode() if base64.b64decode(entry).startswith(b'PATH=') else entry for entry in payload['environment']]
        request = (json.dumps(payload) + '\n').encode()
    completed = subprocess.run([binary, 'worker', '--root', root, 'wire'], input=request+sys.stdin.buffer.read())
    sys.exit(255 if action == 'execute' and mode in ['execute-lost', 'execute-status-lost'] else completed.returncode)
os.execv(binary, [binary, 'worker', '--root', root]+args)
`, 0o755)
	if out, stderr, status := recoveryCommand(t, root, executable, "worker", "--root", filepath.Join(root, "worker"), "init"); status != 0 {
		t.Fatalf("worker init: %s %s", out, stderr)
	}
	return root, home, repo, adapter, executable
}

func recoveryCommand(t *testing.T, directory, executable string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(executable, args...)
	cmd.Dir = directory
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatal(err)
		}
	}
	return stdout.String(), stderr.String(), exitStatus(err)
}

func recoveryWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}
