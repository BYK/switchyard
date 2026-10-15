package switchyard

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fixture(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		".gitignore": ".env\nnode_modules/\n",
		".env":       "PRIVATE_SENTINEL_DO_NOT_COPY\n",
		"README.md":  "original\n",
		"new.txt":    "new untracked file\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git := exec.Command("git", "init", "-q", root)
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s (%v)", output, err)
	}
	git = exec.Command("git", "-C", root, "add", ".gitignore", "README.md")
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("git add: %s (%v)", output, err)
	}
	return root
}

func binary(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	module := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	path := filepath.Join(t.TempDir(), "switchyard")
	cmd := exec.Command("go", "build", "-o", path, "./cmd/switchyard")
	cmd.Dir = module
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %s (%v)", output, err)
	}
	return path
}

func command(t *testing.T, exe, home string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(output)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(output)
	}
	t.Fatalf("start CLI: %v", err)
	return 0, ""
}

func jobID(t *testing.T, output string) string {
	t.Helper()
	id := regexp.MustCompile(`Job ([0-9a-f]{32})`).FindStringSubmatch(output)
	if len(id) != 2 {
		t.Fatal("job did not report a valid ID")
	}
	return id[1]
}

func TestCaptureFreezesWIPAndRejectsPrivateInputs(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	chosen, err := selectFiles(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !equalPaths(chosen.Paths, []string{".gitignore", "README.md", "new.txt"}) {
		t.Fatalf("unexpected selection: %q", chosen.Paths)
	}
	if !equalPaths(chosen.Excluded, []string{".env"}) {
		t.Fatalf("ignored root secret was not listed as excluded: %q", chosen.Excluded)
	}
	if _, err := selectFiles(root, []string{".env"}); err == nil {
		t.Fatal("private include was accepted")
	}
	first, _, err := captureFiles(root, nil, home)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(first.Tree))
	second, _, err := captureFiles(root, nil, home)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(second.Tree))
	if first.Snapshot != second.Snapshot || first.Tree == second.Tree {
		t.Fatal("concurrent captures must share identity but not transfer storage")
	}
	if _, err := os.Stat(filepath.Join(first.Tree, ".env")); !os.IsNotExist(err) {
		t.Fatal("private .env entered the snapshot")
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("later edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyTree(first.Tree); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(first.Tree, "README.md"))
	if err != nil || string(original) != "original\n" {
		t.Fatal("capture changed after source was edited")
	}
	if err := os.WriteFile(filepath.Join(second.Tree, "new.txt"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyTree(second.Tree); err == nil {
		t.Fatal("worker accepted a tampered snapshot")
	}
}

func TestPressureAndCapabilityPlacement(t *testing.T) {
	tests := []struct {
		cpu, memory string
		busy        bool
		reason      string
	}{
		{"some avg10=19.99 avg60=30.00\n", "MemTotal: 1000 kB\nMemAvailable: 800 kB\n", false, "local resources available"},
		{"some avg10=20.00 avg60=0.00\n", "MemTotal: 1000 kB\nMemAvailable: 800 kB\n", true, "CPU contention"},
		{"some avg10=2.00 avg60=0.00\n", "MemTotal: 1000 kB\nMemAvailable: 199 kB\n", true, "low available memory"},
	}
	for _, sample := range tests {
		busy, reason := pressureFrom([]byte(sample.cpu), []byte(sample.memory))
		if busy != sample.busy || reason != sample.reason {
			t.Fatalf("unexpected pressure decision: %t %q", busy, reason)
		}
	}
	home := t.TempDir()
	exe := binary(t)
	t.Setenv("SWITCHYARD_HOME", home)
	t.Setenv("SWITCHYARD_AWS_HOME", filepath.Join(home, "aws-worker"))
	// The default local backend always stays available; explicit capability
	// requests must not fall back to it when it cannot serve the job.
	_, _, err := selectProviderAtPressure(context.Background(), home, exe, []string{"gpu"}, true, "CPU contention")
	if err == nil {
		t.Fatal("GPU capability was silently placed on the local CPU backend")
	}
}

func TestDeletedTrackedFileAndExplicitJJInclude(t *testing.T) {
	root := fixture(t)
	if err := os.Remove(filepath.Join(root, "README.md")); err != nil {
		t.Fatal(err)
	}
	chosen, err := selectFiles(root, nil)
	if err != nil || !equalPaths(chosen.Paths, []string{".gitignore", "new.txt"}) {
		t.Fatalf("tracked deletion leaked into a capture: %v %q", err, chosen.Paths)
	}
	if _, err := exec.LookPath("jj"); err != nil {
		t.Skip("jj is not installed")
	}
	jj := exec.Command("jj", "git", "init", "--colocate", ".")
	jj.Dir = root
	if output, err := jj.CombinedOutput(); err != nil {
		t.Fatalf("jj init: %v %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".env\nignored.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("explicit fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	chosen, err = selectFiles(root, []string{"ignored.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if chosen.VCS != "jj" || !equalPaths(chosen.Paths, []string{".gitignore", "ignored.txt", "new.txt"}) {
		t.Fatalf("jj selection did not include exact WIP: %q", chosen.Paths)
	}
	snapshot, _, err := captureFiles(root, []string{"ignored.txt"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(snapshot.Tree))
	if _, err := verifyTree(snapshot.Tree); err != nil {
		t.Fatal(err)
	}
}

func TestPatchIgnoresLargeWorkerPnpmStore(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original")
	checkout := filepath.Join(root, "checkout")
	for _, dir := range []string{original, checkout, filepath.Join(checkout, ".pnpm-store", "v10", "files")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{original, checkout} {
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("baseline\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(checkout, "agent-note.txt"), []byte("worker edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(checkout, ".pnpm-store", "v10", "files", "cache")
	if err := os.WriteFile(cache, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(cache, maxFileSize+1); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(root, "changes.patch")
	if reason := collectPatch(original, checkout, patch); reason != "" {
		t.Fatalf("cache blocked patch collection: %s", reason)
	}
	data, err := os.ReadFile(patch)
	if err != nil || !strings.Contains(string(data), "+worker edit") || strings.Contains(string(data), ".pnpm-store") {
		t.Fatalf("unexpected patch for worker edit and cache: %v", err)
	}
}

func TestLocalJobExitEditsAndIsolation(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	status, output := command(t, exe, home, "run", "--backend", "local", "--repo", root,
		"--", "sh", "-c", "test ! -e .env && test -f new.txt && printf 'worker edit\\n' > agent-note.txt && printf 'WORKER_PRIVATE_SENTINEL' > .env.local; exit 37")
	if status != 37 || !strings.Contains(output, "exit 37") {
		t.Fatalf("worker status not preserved: %d %s", status, output)
	}
	if _, err := os.Stat(filepath.Join(root, "agent-note.txt")); !os.IsNotExist(err) {
		t.Fatal("worker changed the live source tree")
	}
	runs, err := os.ReadDir(filepath.Join(home, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	var job string
	for _, item := range runs {
		if strings.HasSuffix(item.Name(), ".json") {
			job = strings.TrimSuffix(item.Name(), ".json")
		}
	}
	if job == "" {
		t.Fatal("missing job record")
	}
	patch, err := os.ReadFile(filepath.Join(home, "runs", job+".patch"))
	if err != nil || !strings.Contains(string(patch), "+worker edit") || strings.Contains(string(patch), "PRIVATE_SENTINEL") || strings.Contains(string(patch), ".env.local") {
		t.Fatalf("unexpected review patch: %v %s", err, patch)
	}
	status, output = command(t, exe, home, "status", job)
	if status != 0 || !strings.Contains(output, "exit 37") {
		t.Fatalf("worker status query failed: %d %s", status, output)
	}
	status, output = command(t, exe, home, "run", "--backend", "local", "--repo", root, "--", "sh", "-c", "test -f new.txt")
	if status != 0 || !strings.Contains(output, "exit 0") {
		t.Fatalf("second run failed after seeding mirror: %d %s", status, output)
	}
}

func TestRunForwardsInvocationEnvironmentWithoutWorkerInheritance(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	t.Setenv("SWITCHYARD_WORKER_ONLY_TEST", "worker-only")
	value := "invocation value with 'quotes' and\na newline"
	cmd := exec.Command(exe, "run", "--backend", "local", "--repo", root,
		"--", "sh", "-c", `test "$SWITCHYARD_INVOCATION_TEST" = "$1" && test -z "${SWITCHYARD_WORKER_ONLY_TEST+x}" && test "$SWITCHYARD_BACKEND" = local && test "$PWD" = "$(pwd)"`, "switchyard-test", value)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "SWITCHYARD_WORKER_ONLY_TEST=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "SWITCHYARD_HOME="+home, "SWITCHYARD_INVOCATION_TEST="+value)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker did not receive the invocation environment: %v %s", err, output)
	}
	job := jobID(t, string(output))
	record, err := os.ReadFile(filepath.Join(home, "runs", job+".json"))
	if err != nil || strings.Contains(string(record), value) || strings.Contains(string(output), value) {
		t.Fatal("invocation environment appeared in job metadata or output")
	}
}

func TestEnvironmentValidationAndWorkerOverrides(t *testing.T) {
	for _, environment := range [][]string{{"INVALID"}, {"=value"}, {"A=first", "A=second"}, {"A=nul\x00byte"}} {
		if validateEnvironment(environment) == nil {
			t.Fatal("invalid invocation environment was accepted")
		}
	}
	environment := workerEnvironment([]string{"A=quoted 'value'\nand newline", "PWD=/source", "SWITCHYARD_BACKEND=remote"}, "/worker/checkout")
	if !equalPaths(environment, []string{"A=quoted 'value'\nand newline", "PWD=/worker/checkout", "SWITCHYARD_BACKEND=local"}) {
		t.Fatal("worker did not preserve invocation values and replace worker-specific state")
	}
}

func TestCommandReturnsExitStatusWhileStdinRemainsOpen(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	for _, expected := range []int{0, 37} {
		input, holdOpen, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "run", "--backend", "local", "--repo", root,
			"--", "sh", "-c", fmt.Sprintf("exit %d", expected))
		cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home)
		cmd.Stdin = input
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			input.Close()
			holdOpen.Close()
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() { result <- cmd.Wait() }()
		select {
		case err := <-result:
			input.Close()
			holdOpen.Close()
			if status := exitStatus(err); status != expected {
				t.Fatalf("command exited %d, want %d with open stdin", status, expected)
			}
		case <-time.After(5 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			input.Close()
			holdOpen.Close()
			<-result
			t.Fatal("Switchyard waited for terminal stdin after the command exited")
		}
	}
}

func TestNodeJobsInstallInEachCheckoutAndReportSetupFailures(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	for name, value := range map[string]string{
		"package.json":   `{"name":"switchyard-node-fixture","private":true,"scripts":{"test":"node test.js"}}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\nimporters:\n  .: {}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools := t.TempDir()
	trace := filepath.Join(t.TempDir(), "install-trace")
	pnpm := `#!/bin/sh
if [ "$1" = install ]; then
  printf 'install\n' >> "$SWITCHYARD_TEST_INSTALL_TRACE"
  [ "$SWITCHYARD_TEST_INSTALL_FAIL" != 1 ] || exit 29
  mkdir -p node_modules
  printf 'ready\n' > node_modules/fixture
  exit 0
fi
[ "$1" = test ] && [ -f node_modules/fixture ] || exit 91
printf 'node command ran\n'
printf 'agent edit\n' > agent-note.txt
exit 37
`
	if err := os.WriteFile(filepath.Join(tools, "pnpm"), []byte(pnpm), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SWITCHYARD_TEST_INSTALL_TRACE", trace)
	for run := 0; run < 2; run++ {
		status, output := command(t, exe, home, "run", "--backend", "local", "--repo", root, "--", "pnpm", "test")
		if status != 37 || !strings.Contains(output, "node command ran") {
			t.Fatalf("remote Node command did not run after install: %d %s", status, output)
		}
	}
	contents, err := os.ReadFile(trace)
	if err != nil || string(contents) != "install\ninstall\n" {
		t.Fatalf("each private checkout needs its own install: %v %q", err, contents)
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules")); !os.IsNotExist(err) {
		t.Fatal("Node setup wrote to the live source")
	}
	t.Setenv("SWITCHYARD_TEST_INSTALL_FAIL", "1")
	status, output := command(t, exe, home, "run", "--backend", "local", "--repo", root, "--", "pnpm", "test")
	if status != 2 || !strings.Contains(output, "dependency setup") || strings.Contains(output, "node command ran") {
		t.Fatalf("failed setup was misreported as a command result: %d %s", status, output)
	}
	job := jobID(t, output)
	status, output = command(t, exe, home, "status", job)
	if status != 0 || !strings.Contains(output, "setup-failed") || !strings.Contains(output, "exit unknown") {
		t.Fatalf("failed setup has an invalid status: %d %s", status, output)
	}
}

func TestNpmJobUsesCleanInstall(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	for name, content := range map[string]string{
		"package.json":      `{"name":"switchyard-npm-fixture","private":true}`,
		"package-lock.json": `{"name":"switchyard-npm-fixture","lockfileVersion":3,"packages":{"":{}}}`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools := t.TempDir()
	npm := `#!/bin/sh
if [ "$1" = ci ] && [ "$2" = --prefer-offline ]; then
  mkdir node_modules
  exit 0
fi
[ "$1" = test ] && [ -d node_modules ] || exit 91
printf 'npm ran after ci\n'
exit 27
`
	if err := os.WriteFile(filepath.Join(tools, "npm"), []byte(npm), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	status, output := command(t, exe, home, "run", "--backend", "local", "--repo", root, "--", "npm", "test")
	if status != 27 || !strings.Contains(output, "npm ran after ci") {
		t.Fatalf("npm job skipped its clean install: %d %s", status, output)
	}
}

func TestProxyKeepsStandardStreamsSeparate(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	worker := filepath.Join(t.TempDir(), "worker")
	status, output := command(t, exe, home, "backend", "add", "--name", "sandbox", "--kind", "local", "--root", worker)
	if status != 0 {
		t.Fatalf("configure worker: %s", output)
	}
	cmd := exec.Command(exe, "proxy", "--backend", "sandbox", "--repo", root, "--", "sh", "-c", `IFS= read -r input; printf 'stdout:%s\n' "$input"; printf 'stderr\n' >&2; exit 37`)
	cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home)
	cmd.Stdin = strings.NewReader("from stdin\n")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if code, ok := err.(*exec.ExitError); !ok || code.ExitCode() != 37 {
		t.Fatalf("proxy exit did not match remote command: %v", err)
	}
	if stdout.String() != "stdout:from stdin\n" || stderr.String() != "stderr\n" {
		t.Fatalf("proxy changed command streams: stdout %q stderr %q", stdout.String(), stderr.String())
	}
	status, jobs := command(t, exe, home, "jobs")
	if status != 0 || !strings.Contains(jobs, "sandbox snapshot=") {
		t.Fatalf("silent proxy lost the job ID: %d %q", status, jobs)
	}
}

func TestProxyLocalRunsOriginalCommandWithoutCapturing(t *testing.T) {
	home := t.TempDir()
	exe := binary(t)
	cmd := exec.Command(exe, "proxy", "--backend", "local", "--repo", t.TempDir(), "--", "sh", "-c", "printf local-only; exit 19")
	cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if code, ok := err.(*exec.ExitError); !ok || code.ExitCode() != 19 {
		t.Fatalf("local proxy lost the command exit: %v", err)
	}
	if stdout.String() != "local-only" || stderr.Len() != 0 {
		t.Fatalf("local proxy changed command output: stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{"projects", "runs"} {
		if _, err := os.Stat(filepath.Join(home, directory)); !os.IsNotExist(err) {
			t.Fatalf("local proxy created unnecessary %s storage", directory)
		}
	}
}

func TestShellEntrypointRoutesAbsoluteToolAndPreservesOtherShellSyntax(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	worker := filepath.Join(t.TempDir(), "worker")
	if err := saveBackend(home, backendConfig{Name: "sandbox", Kind: "local", Root: worker}); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"package.json": `{"name":"shell-node-fixture","private":true}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'\nimporters:\n  .: {}\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tool := filepath.Join(t.TempDir(), "pnpm")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nif [ \"$1\" = install ]; then mkdir node_modules; exit 0; fi\ntest -d node_modules || exit 91\nprintf 'from worker\\n'\nprintf 'remote edit\\n' > agent-note.txt\nexit 37\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	shell := filepath.Join(t.TempDir(), "switchyard-shell")
	if err := os.Symlink(exe, shell); err != nil {
		t.Fatal(err)
	}
	run := func(script string) (int, string, string) {
		cmd := exec.Command(shell, "-c", script)
		cmd.Dir, cmd.Env = root, append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_BACKEND=sandbox", "PATH="+filepath.Dir(tool)+":"+os.Getenv("PATH"))
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil {
			return 0, stdout.String(), stderr.String()
		}
		if code, ok := err.(*exec.ExitError); ok {
			return code.ExitCode(), stdout.String(), stderr.String()
		}
		t.Fatalf("shell did not start: %v", err)
		return 2, "", ""
	}
	status, stdout, stderr := run(tool + " test")
	if status != 37 || stdout != "from worker\n" || stderr != "" {
		t.Fatalf("absolute tool was not transparently routed: %d %q %q", status, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "agent-note.txt")); !os.IsNotExist(err) {
		t.Fatal("remote edits reached the live source")
	}
	status, stdout, stderr = run("printf 'local' | tr a-z A-Z")
	if status != 0 || stdout != "LOCAL" || stderr != "" {
		t.Fatalf("unrecognized shell command changed: %d %q %q", status, stdout, stderr)
	}
	if _, err := exec.LookPath("node"); err == nil {
		child := exec.Command("node", "-e", `const {spawnSync} = require('node:child_process'); const r = spawnSync(process.argv[1] + ' test', [], {shell: process.argv[2], encoding: 'utf8', env: {...process.env, PATH: process.argv[3] + ':' + process.env.PATH}}); process.stdout.write(r.stdout); process.stderr.write(r.stderr); process.exit(r.status)`, tool, shell, filepath.Dir(tool))
		child.Dir, child.Env = root, append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_BACKEND=sandbox", "PATH="+filepath.Dir(tool)+":"+os.Getenv("PATH"))
		var output, errorOutput strings.Builder
		child.Stdout, child.Stderr = &output, &errorOutput
		if err := child.Run(); err == nil || err.(*exec.ExitError).ExitCode() != 37 || output.String() != "from worker\n" || errorOutput.Len() != 0 {
			t.Fatalf("Node shell option changed routed command: %v %q %q", err, output.String(), errorOutput.String())
		}
	}
}

func TestRealPnpmBuildUsesWorkerCheckout(t *testing.T) {
	if _, err := exec.LookPath("pnpm"); err != nil {
		t.Skip("pnpm is not installed")
	}
	root := fixture(t)
	for name, content := range map[string]string{
		"package.json":   `{"name":"switchyard-node-build","private":true,"scripts":{"build":"node build.mjs"}}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\nsettings:\n  autoInstallPeers: true\n  excludeLinksFromLockfile: false\nimporters:\n  .: {}\n",
		"build.mjs":      `import { mkdirSync, writeFileSync } from 'node:fs'; mkdirSync('dist'); writeFileSync('dist/result.txt', 'built on worker'); console.log('built')`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	exe := binary(t)
	status, output := command(t, exe, home, "run", "--backend", "local", "--repo", root, "--", "pnpm", "build")
	if status != 0 || !strings.Contains(output, "built") {
		t.Fatalf("pnpm build did not run: %d %s", status, output)
	}
	if _, err := os.Stat(filepath.Join(root, "dist")); !os.IsNotExist(err) {
		t.Fatal("build output appeared in the live source")
	}
	patch, err := os.ReadFile(filepath.Join(home, "runs", jobID(t, output)+".patch"))
	if err != nil || !strings.Contains(string(patch), "dist/result.txt") || strings.Contains(string(patch), "node_modules") {
		t.Fatalf("build output was not saved without dependencies: %v", err)
	}
}

func TestWorkerPrunesOldJobsAndMirrors(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	status, output := command(t, exe, home, "run", "--backend", "local", "--repo", root, "--", "sh", "-c", "true")
	if status != 0 {
		t.Fatal(output)
	}
	id := jobID(t, output)
	content, err := os.ReadFile(filepath.Join(home, "runs", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record localJob
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatal(err)
	}
	previous := time.Now().Add(-8 * 24 * time.Hour)
	base := filepath.Join(home, "worker", "projects", record.Project)
	abandoned := filepath.Join(base, "jobs", strings.Repeat("f", 32))
	if err := os.MkdirAll(filepath.Join(abandoned, "incoming", "files"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(abandoned, previous, previous); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(base, "mirrors", "mirror-abandoned")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(staging, previous, previous); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(home, "runs", id+".json"),
		filepath.Join(base, "jobs", id),
	} {
		if err := os.Chtimes(path, previous, previous); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("next version\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, output = command(t, exe, home, "run", "--backend", "local", "--repo", root, "--", "sh", "-c", "test -f new.txt")
	if status != 0 {
		t.Fatal(output)
	}
	for _, path := range []string{
		filepath.Join(home, "runs", id+".json"),
		filepath.Join(base, "jobs", id),
		abandoned,
		staging,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("old job was not reclaimed: %s: %v", path, err)
		}
	}
	mirrors, err := os.ReadDir(filepath.Join(home, "worker", "projects", record.Project, "mirrors"))
	if err != nil || len(mirrors) != 1 {
		t.Fatalf("worker retains old mirrors: %d, %v", len(mirrors), err)
	}
}

func TestWorkerRootRefusesUnrelatedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "important"), []byte("retain me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := initWorker(root); err == nil {
		t.Fatal("worker took over a nonempty unrelated directory")
	}
	if err := os.WriteFile(filepath.Join(root, ".switchyard-worker"), []byte("wrong owner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := initWorker(root); err == nil {
		t.Fatal("worker replaced an invalid ownership marker")
	}
}

func TestCancelStopsRunningProcessGroup(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	run := exec.Command(exe, "run", "--backend", "local", "--repo", root,
		"--", "sh", "-c", "printf 'child started\\n'; sleep 30")
	run.Env = append(os.Environ(), "SWITCHYARD_HOME="+home)
	stdout, err := run.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		run.Process.Kill()
		run.Wait()
	})
	seen := make(chan string, 20)
	go func() {
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			seen <- scan.Text()
		}
		close(seen)
	}()
	var job string
	for job == "" {
		select {
		case line, open := <-seen:
			if !open {
				t.Fatal("job exited before reporting its ID")
			}
			if strings.HasPrefix(line, "Job ") {
				job = strings.Split(strings.TrimPrefix(line, "Job "), ";")[0]
			}
		case <-time.After(12 * time.Second):
			t.Fatal("job did not start")
		}
	}
	// The worker may still be setting up its isolated checkout after the ID
	// appears. Wait until it reports a running child before signalling it.
	for attempt := 0; attempt < 100; attempt++ {
		_, result := command(t, exe, home, "status", job)
		if strings.Contains(result, ": running;") {
			break
		}
		if attempt == 99 {
			t.Fatal("worker never reported a running process")
		}
		<-time.After(20 * time.Millisecond)
	}
	status, response := command(t, exe, home, "cancel", job)
	if status != 0 || !strings.Contains(response, "running") {
		t.Fatalf("could not request cancellation: %d %s", status, response)
	}
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case err := <-done:
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 143 {
			t.Fatalf("canceled command exit status: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("canceled command did not exit")
	}
	status, response = command(t, exe, home, "status", job)
	if status != 0 || !strings.Contains(response, "finished") || !strings.Contains(response, "exit 143") {
		t.Fatalf("canceled job status did not settle: %d %s", status, response)
	}
}

func TestCancelDuringNodeSetupNeverStartsRequestedCommand(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	for name, content := range map[string]string{"package.json": `{"name":"setup-cancel","private":true}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'\nimporters:\n  .: {}\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tools := t.TempDir()
	pnpm := "#!/bin/sh\nif [ \"$1\" = install ]; then printf 'install started\\n'; exec sleep 30; fi\nprintf 'REQUESTED_COMMAND_RAN\\n'\n"
	if err := os.WriteFile(filepath.Join(tools, "pnpm"), []byte(pnpm), 0o700); err != nil {
		t.Fatal(err)
	}
	run := exec.Command(exe, "run", "--backend", "local", "--repo", root, "--", "pnpm", "test")
	run.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "PATH="+tools+":"+os.Getenv("PATH"))
	stdout, err := run.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { run.Process.Kill(); run.Wait() })
	seen := make(chan string, 20)
	go func() {
		scan := bufio.NewScanner(stdout)
		for scan.Scan() {
			seen <- scan.Text()
		}
		close(seen)
	}()
	var job string
	for {
		select {
		case line, open := <-seen:
			if !open {
				t.Fatal("setup exited before it could be canceled")
			}
			if strings.HasPrefix(line, "Job ") {
				job = strings.Split(strings.TrimPrefix(line, "Job "), ";")[0]
			}
			if line == "install started" {
				goto cancel
			}
		case <-time.After(12 * time.Second):
			t.Fatal("setup did not start")
		}
	}
cancel:
	if job == "" {
		t.Fatal("job ID missing before setup")
	}
	status, output := command(t, exe, home, "cancel", job)
	if status != 0 || !strings.Contains(output, "running") {
		t.Fatalf("could not cancel setup: %d %s", status, output)
	}
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case err := <-done:
		if code, ok := err.(*exec.ExitError); !ok || code.ExitCode() != 143 {
			t.Fatalf("setup cancellation exit was not the signal: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("canceled setup did not exit")
	}
	for line := range seen {
		if line == "REQUESTED_COMMAND_RAN" {
			t.Fatal("requested command ran after canceled setup")
		}
	}
	status, output = command(t, exe, home, "status", job)
	if status != 0 || !strings.Contains(output, "setup-failed") || !strings.Contains(output, "exit unknown") {
		t.Fatalf("setup cancellation was recorded as a command result: %d %s", status, output)
	}
}

func TestHookPreservesLocalCommandAndRoutesConfiguredBackend(t *testing.T) {
	if !equalPaths(shellCommand("pnpm --version"), []string{"pnpm", "--version"}) {
		t.Fatal("shell adapter still restricts actions by subcommand")
	}
	if !equalPaths(shellCommand("make"), []string{"make"}) {
		t.Fatal("shell adapter rejected a bare build command")
	}
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	status, output := command(t, exe, home, "hook", "install")
	if status != 0 {
		t.Fatalf("could not install shims: %s", output)
	}
	for _, name := range []string{"switchyard-shell"} {
		if link, err := os.Readlink(filepath.Join(shimDirectory(home), name)); err != nil || link != exe {
			t.Fatalf("hook install omitted %s: %v", name, err)
		}
	}
	toolDir := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(toolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(toolDir, "pnpm")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nif [ \"$1\" = '--version' ]; then exit 23; fi\nif [ \"$1\" = install ]; then mkdir node_modules; exit 0; fi\ntest -d node_modules || exit 91\nprintf 'worker edit\\n' > agent-note.txt\nexit 37\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"package.json": `{"name":"hook-node-fixture","private":true}`, "pnpm-lock.yaml": "lockfileVersion: '9.0'\nimporters:\n  .: {}\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	path := shimDirectory(home) + ":" + toolDir + ":" + os.Getenv("PATH")
	shim := filepath.Join(shimDirectory(home), "pnpm")
	local := exec.Command(shim, "--version")
	local.Dir, local.Env = root, append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_BACKEND=local", "PATH="+path)
	if err := local.Run(); err == nil || err.(*exec.ExitError).ExitCode() != 23 {
		t.Fatalf("hook failed to preserve original tool exit: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, "worker")); !os.IsNotExist(err) {
		t.Fatal("non-build invocation initialized a worker")
	}
	if err := saveBackend(home, backendConfig{Name: "fixture", Kind: "local", Root: filepath.Join(t.TempDir(), "worker")}); err != nil {
		t.Fatal(err)
	}
	remote := exec.Command(shim, "test")
	remote.Dir, remote.Env = root, append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_BACKEND=fixture", "PATH="+path)
	result, err := remote.CombinedOutput()
	if err == nil || err.(*exec.ExitError).ExitCode() != 37 || !strings.Contains(string(result), "exit 37") {
		t.Fatalf("eligible command was not routed: %v %s", err, result)
	}
	if _, err := os.Stat(filepath.Join(root, "agent-note.txt")); !os.IsNotExist(err) {
		t.Fatal("hook applied worker edits to source")
	}
	if script, err := exec.LookPath("script"); err == nil {
		env := append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_BACKEND=fixture", "PATH="+path, "SHELL=/usr/bin/bash")
		shimTTY := exec.Command(script, "-q", "-e", "-c", shim+" test", "/dev/null")
		shimTTY.Dir, shimTTY.Env = root, env
		if result, err := shimTTY.CombinedOutput(); err == nil || err.(*exec.ExitError).ExitCode() != 91 {
			t.Fatalf("PATH shim routed a TTY command: %v %q", err, result)
		}
		shellTTY := exec.Command(script, "-q", "-e", "-c", exe+" hook --shell --resolved "+tool+" -- pnpm test", "/dev/null")
		shellTTY.Dir, shellTTY.Env = root, env
		if result, err := shellTTY.CombinedOutput(); err == nil || err.(*exec.ExitError).ExitCode() != 37 || !strings.Contains(string(result), "exit 37") {
			t.Fatalf("Bash hook did not route a TTY command: %v %q", err, result)
		}
		if _, err := os.Stat(filepath.Join(root, "agent-note.txt")); !os.IsNotExist(err) {
			t.Fatal("Bash hook applied worker edits to source")
		}
	}
	alternative := filepath.Join(toolDir, "alternate-pnpm")
	if err := os.WriteFile(alternative, []byte("#!/bin/sh\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"auto", "fixture"} {
		selected := exec.Command(exe, "hook", "--shell", "--resolved", alternative, "--", "pnpm", "test")
		selected.Dir, selected.Env = root, append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_BACKEND="+backend, "PATH="+path)
		result, err := selected.CombinedOutput()
		expected := 44
		if backend == "fixture" {
			expected = 2
		}
		if err == nil || err.(*exec.ExitError).ExitCode() != expected {
			t.Fatalf("Bash's resolved command was replaced (backend %s): %v %q", backend, err, result)
		}
		if backend == "fixture" && !strings.Contains(string(result), "cannot offload") {
			t.Fatalf("forced backend did not explain the mismatched command: %q", result)
		}
	}
}

func TestSSHProviderTransfersAndPinsSnapshot(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/sshd"); err != nil {
		t.Skip("OpenSSH server is not installed")
	}
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	server := t.TempDir()
	clientKey := filepath.Join(server, "client")
	hostKey := filepath.Join(server, "host")
	for _, key := range []string{clientKey, hostKey} {
		cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create test key: %v %s", err, output)
		}
	}
	pub, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(server, "authorized_keys")
	if err := os.WriteFile(allowed, pub, 0o600); err != nil {
		t.Fatal(err)
	}
	pub, err = os.ReadFile(hostKey + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(server, "known_hosts")
	keyParts := strings.Fields(string(pub))
	if err := os.WriteFile(known, []byte("switchyard-test "+keyParts[0]+" "+keyParts[1]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("loopback sockets are unavailable")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nAuthorizedKeysFile %s\nPasswordAuthentication no\nPubkeyAuthentication yes\nStrictModes no\nUsePAM no\nPidFile %s\nLogLevel ERROR\n", port, hostKey, allowed, filepath.Join(server, "sshd.pid"))
	configPath := filepath.Join(server, "sshd_config")
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	validate := exec.Command("/usr/sbin/sshd", "-t", "-f", configPath)
	if output, err := validate.CombinedOutput(); err != nil {
		t.Skipf("local SSH server could not be configured: %v %s", err, output)
	}
	sshd := exec.Command("/usr/sbin/sshd", "-D", "-e", "-f", configPath)
	if err := sshd.Start(); err != nil {
		t.Skipf("local SSH server cannot start: %v", err)
	}
	t.Cleanup(func() {
		sshd.Process.Kill()
		sshd.Wait()
	})
	for attempt := 0; attempt < 20; attempt++ {
		connection, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 100*time.Millisecond)
		if err == nil {
			connection.Close()
			break
		}
		if attempt == 19 {
			t.Fatal("local SSH server did not listen")
		}
		<-time.After(50 * time.Millisecond)
	}
	worker := filepath.Join(server, "worker")
	status, output := command(t, exe, home, "worker", "--root", worker, "init")
	if status != 0 {
		t.Fatalf("initialize SSH worker: %s", output)
	}
	cfg := backendConfig{Name: "loopback", Kind: "ssh", Root: worker,
		Host: "127.0.0.1", User: account.Username, Port: port, Command: exe,
		Identity: clientKey, KnownHosts: known, HostKeyAlias: "switchyard-test"}
	if err := saveBackend(home, cfg); err != nil {
		t.Fatal(err)
	}
	status, output = command(t, exe, home, "run", "--backend", "loopback", "--repo", root,
		"--", "sh", "-c", "test ! -f .env && test -f new.txt && printf 'ssh edit\\n' > via-ssh.txt; exit 37")
	if status != 37 || !strings.Contains(output, "exit 37") {
		t.Fatalf("SSH job failed: %d %s", status, output)
	}
	if _, err := os.Stat(filepath.Join(root, "via-ssh.txt")); !os.IsNotExist(err) {
		t.Fatal("SSH worker wrote into the source tree")
	}
	proxy := exec.Command(exe, "proxy", "--backend", "loopback", "--repo", root, "--", "sh", "-c", `IFS= read -r input; printf 'ssh-stdout:%s\n' "$input"; printf 'ssh-stderr\n' >&2; exit 37`)
	proxy.Env = append(os.Environ(), "SWITCHYARD_HOME="+home)
	proxy.Stdin = strings.NewReader("over-ssh\n")
	var stdout, stderr strings.Builder
	proxy.Stdout, proxy.Stderr = &stdout, &stderr
	if err := proxy.Run(); err == nil || err.(*exec.ExitError).ExitCode() != 37 {
		t.Fatalf("SSH proxy lost remote exit status: %v", err)
	}
	if stdout.String() != "ssh-stdout:over-ssh\n" || stderr.String() != "ssh-stderr\n" {
		t.Fatalf("SSH proxy lost standard streams: stdout %q stderr %q", stdout.String(), stderr.String())
	}
	value := "synthetic quoted 'value' with\na newline"
	forwarded := exec.Command(exe, "run", "--backend", "loopback", "--repo", root,
		"--", "sh", "-c", `test "$SWITCHYARD_SSH_ENV_TEST" = "$1" && test "$SWITCHYARD_BACKEND" = local && test "$PWD" = "$(pwd)"`, "switchyard-test", value)
	forwarded.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_SSH_ENV_TEST="+value)
	if output, err := forwarded.CombinedOutput(); err != nil {
		t.Fatalf("SSH worker lost the invocation environment: %v %s", err, output)
	}
	if err := os.WriteFile(known, []byte("switchyard-test ssh-ed25519 WRONGKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, output = command(t, exe, home, "run", "--backend", "loopback", "--repo", root, "--", "sh", "-c", "true")
	if status == 0 || !strings.Contains(output, "not ready") {
		t.Fatalf("SSH accepted a changed host key: %d %s", status, output)
	}
}

func TestExternalProviderContract(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	t.Setenv("SWITCHYARD_HOME", home)
	t.Setenv("SWITCHYARD_AWS_HOME", filepath.Join(home, "aws-worker"))
	exe := binary(t)
	worker := filepath.Join(t.TempDir(), "worker")
	status, output := command(t, exe, home, "worker", "--root", worker, "init")
	if status != 0 {
		t.Fatalf("initialize plugin worker: %s", output)
	}
	adapter := filepath.Join(t.TempDir(), "adapter")
	script := fmt.Sprintf(`#!/bin/sh
  if [ "$1" = sync ]; then
  shift
  [ "$1" = --root ] || exit 2
  root="$2"; shift 2
  [ "$1" = --project ] || exit 2
  project="$2"; shift 2
  [ "$1" = --job ] || exit 2
  job="$2"; shift 2
  [ "$1" = --source ] || exit 2
  source="$2"
  exec /usr/bin/rsync -a --checksum --delete -- "$source/" "$root/projects/$project/jobs/$job/incoming/files/"
fi
export SWITCHYARD_WORKER_ONLY_TEST=worker-only
PATH=/usr/bin:/bin
export PATH
exec %q "$@"
`, exe)
	if err := os.WriteFile(adapter, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := backendConfig{Name: "custom", Kind: "plugin", Root: worker, Command: adapter}
	if err := saveBackend(home, cfg); err != nil {
		t.Fatal(err)
	}
	// Pressure must bootstrap remote execution even for an unseen command.
	// Requiring timings here makes the first remote sample impossible.
	if name := pressureReadyBackendAtPressure(home, exe, true); name != "custom" {
		t.Fatalf("pressure did not route a command with no timing history: %q", name)
	}
	auto, _, err := selectProviderAtPressure(context.Background(), home, exe, []string{"cpu"}, true, "CPU contention")
	if err != nil || auto.Name() != "custom" {
		t.Fatalf("pressure did not select the ready plugin: %v (%s)", err, auto.Name())
	}
	auto, _, err = selectProviderAtPressure(context.Background(), home, exe, []string{"cpu"}, false, "local resources available")
	if err != nil || auto.Name() != "local" {
		t.Fatalf("idle placement did not stay local: %v (%s)", err, auto.Name())
	}
	status, output = command(t, exe, home, "run", "--backend", "custom", "--require", "gpu", "--repo", root, "--", "sh", "-c", "true")
	if status != 2 || !strings.Contains(output, "required capabilities") {
		t.Fatalf("unsupported plugin capability was accepted: %d %s", status, output)
	}
	status, output = command(t, exe, home, "run", "--backend", "custom", "--repo", root,
		"--", "sh", "-c", "test ! -f .env && test -f new.txt; exit 37")
	if status != 37 || !strings.Contains(output, "exit 37") {
		t.Fatalf("provider plugin failed: %d %s", status, output)
	}
	path, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	signature, ok := commandSignature(root, path, []string{"-c", "test ! -f .env && test -f new.txt; exit 37"})
	if !ok {
		t.Fatal("plugin command observation could not be identified")
	}
	history, err := readObservations(home)
	if err != nil || len(history[signature].Backends[observedBackendID(shellProvider{cfg: cfg})]) != 1 {
		t.Fatalf("completed plugin job timing was not learned: %v", err)
	}
	for range 2 {
		if err := recordCommandObservation(home, signature, "local", 6*time.Second, 4*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordCommandObservation(home, signature, observedBackendID(shellProvider{cfg: cfg}), time.Millisecond, 0); err != nil {
		t.Fatal(err)
	}
	if name := pressureReadyBackendAtPressure(home, exe, true); name != "custom" {
		t.Fatalf("ready plugin was not selected under pressure: %q", name)
	}
	if name := pressureReadyBackendAtPressure(home, exe, false); name != "" {
		t.Fatalf("idle host selected a remote backend: %q", name)
	}
	pluginRun := exec.Command(exe, "run", "--backend", "custom", "--repo", root,
		"--", "sh", "-c", `test "$SWITCHYARD_PLUGIN_ENV_TEST" = 'from invocation' && test -z "${SWITCHYARD_WORKER_ONLY_TEST+x}" && test "$SWITCHYARD_BACKEND" = local`)
	pluginRun.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_PLUGIN_ENV_TEST=from invocation")
	if output, err := pluginRun.CombinedOutput(); err != nil {
		t.Fatalf("plugin worker did not isolate the invocation environment: %v %s", err, output)
	}
	tools := filepath.Join(t.TempDir(), "tools")
	if err := os.Mkdir(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(tools, "switchyard-env-probe")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ntest \"$SWITCHYARD_PLUGIN_ENV_TEST\" = 'from invocation'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	fromPath := exec.Command(exe, "run", "--backend", "custom", "--repo", root,
		"--", "switchyard-env-probe")
	fromPath.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_PLUGIN_ENV_TEST=from invocation",
		"PATH="+tools+":"+os.Getenv("PATH"))
	if output, err := fromPath.CombinedOutput(); err != nil {
		t.Fatalf("worker did not resolve a tool using the invocation PATH: %v %s", err, output)
	}
}

func TestAutomaticShellPreservesNativeFallback(t *testing.T) {
	home, root, tools := t.TempDir(), t.TempDir(), t.TempDir()
	exe := binary(t)
	tool := filepath.Join(tools, "pnpm")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nIFS= read -r input\nprintf 'native:%s:%s\\n' \"$input\" \"$PWD\"\nprintf 'native stderr\\n' >&2\nexit 37\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "shell", "--repo", root, "--", "-c", "pnpm test")
	cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_BACKEND=auto", "SWITCHYARD_TRACE_PREVIEWED=", "PATH="+tools+":"+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader("stdin preserved\n")
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 {
		t.Fatalf("automatic shell lost the native exit status: %v %s", err, output)
	}
	if !strings.Contains(string(output), "native:stdin preserved:"+root) || !strings.Contains(string(output), "native stderr") {
		t.Fatalf("automatic shell lost streams or working directory: %s", output)
	}
	if _, err := os.Stat(filepath.Join(home, "runs")); !os.IsNotExist(err) {
		t.Fatal("a non-worktree native command created remote job records")
	}
}

func TestAutomaticShellPreservesAbsoluteLocalExecutable(t *testing.T) {
	home, root, tools := t.TempDir(), fixture(t), t.TempDir()
	exe := binary(t)
	tool := filepath.Join(root, "pnpm")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf 'correct absolute executable\\n'\nexit 37\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "pnpm"), []byte("#!/bin/sh\nprintf 'wrong PATH executable\\n'\nexit 44\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "shell", "--repo", root, "--", "-c", tool+" test")
	cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_AWS_HOME="+filepath.Join(home, "aws-worker"), "SWITCHYARD_BACKEND=auto", "PATH="+tools+":"+os.Getenv("PATH"))
	output, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 37 || !strings.Contains(string(output), "correct absolute executable") {
		t.Fatalf("automatic fallback changed the resolved executable: %v %s", err, output)
	}
}
