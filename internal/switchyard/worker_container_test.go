package switchyard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestContainerPnpmStoreStaysOutsideTheCheckout(t *testing.T) {
	environment := containerJobEnvironment([]string{
		"npm_config_store_dir=/workspace/.pnpm-store", "NPM_CONFIG_STORE_DIR=/workspace/other-store",
		"PNPM_STORE_DIR=/workspace/third-store", "USER=worker",
	}, "/workspace")
	if strings.Count(strings.Join(environment, "\n"), "npm_config_store_dir=") != 1 ||
		!strings.Contains(strings.Join(environment, "\n"), "npm_config_store_dir=/home/switchyard/.local/share/pnpm/store") ||
		strings.Contains(strings.Join(environment, "\n"), "/workspace/.pnpm-store") ||
		strings.Contains(strings.Join(environment, "\n"), "/workspace/other-store") ||
		strings.Contains(strings.Join(environment, "\n"), "/workspace/third-store") ||
		!strings.Contains(strings.Join(environment, "\n"), "USER=worker") {
		t.Fatal("pnpm store is inside the job checkout or invocation environment was lost")
	}
}

func TestPinnedNodeImageRunsIsolatedSnapshotAndReturnsEdits(t *testing.T) {
	image := os.Getenv("SWITCHYARD_TEST_NODE_IMAGE")
	if image == "" {
		t.Skip("set SWITCHYARD_TEST_NODE_IMAGE to the locally built pinned image ID")
	}
	if parent := os.Getenv("SWITCHYARD_TEST_DOCKER_PARENT"); parent != "" {
		t.Setenv("TMPDIR", parent)
	} else {
		t.Skip("set SWITCHYARD_TEST_DOCKER_PARENT to a directory visible to the Docker daemon")
	}
	if err := inspectImage(image); err != nil {
		t.Fatal(err)
	}
	root, home, exe := fixture(t), t.TempDir(), binary(t)
	selection, err := selectFiles(root, nil)
	if err != nil || !equalPaths(selection.Paths, []string{".gitignore", "README.md", "new.txt"}) || !equalPaths(selection.Excluded, []string{".env"}) {
		t.Fatalf("preview must show every selected and excluded path: %+v %v", selection, err)
	}
	status, preview := command(t, exe, home, "preview", "--repo", root)
	if status != 0 || !strings.Contains(preview, "README.md") || !strings.Contains(preview, ".env") {
		t.Fatalf("preview failed: %d %s", status, preview)
	}
	if err := initWorker(filepath.Join(home, "worker")); err != nil {
		t.Fatal(err)
	}
	if err := saveImage(filepath.Join(home, "worker"), image); err != nil {
		t.Fatal(err)
	}
	code := `const fs = require('node:fs');
const { execFileSync } = require('node:child_process');
if (fs.existsSync('.env') || fs.existsSync('/var/run/docker.sock')) process.exit(91);
if (process.env.SWITCHYARD_BACKEND !== 'local' || process.env.HOME !== '/home/switchyard' || process.env.SWITCHYARD_TEST_ENV !== 'forwarded' || process.env.USER !== 'switchyard-invoker' || process.env.LOGNAME !== 'switchyard-invoker') process.exit(92);
if (execFileSync('jq', ['-er', '.tool'], { input: '{"tool":"jq"}', encoding: 'utf8' }).trim() !== 'jq') process.exit(94);
if (execFileSync('ps', ['-p', String(process.pid), '-o', 'pid='], { encoding: 'utf8' }).trim() !== String(process.pid)) process.exit(95);
process.stdin.setEncoding('utf8');
let content = '';
process.stdin.on('data', chunk => content += chunk);
process.stdin.on('end', () => { if (content !== 'input\n') process.exit(93); fs.writeFileSync('agent-note.txt', 'container edit\n'); console.log('container node complete'); process.exit(37); });`
	cmd := exec.Command(exe, "run", "--backend", "local", "--repo", root, "--", "node", "-e", code)
	cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_TEST_ENV=forwarded", "USER=switchyard-invoker", "LOGNAME=switchyard-invoker")
	cmd.Stdin = strings.NewReader("input\n")
	output, err := cmd.CombinedOutput()
	if exitStatus(err) != 37 || !strings.Contains(string(output), "container node complete") {
		t.Fatalf("container job failed: exit=%d output=%s", exitStatus(err), output)
	}
	if _, err := os.Stat(filepath.Join(root, "agent-note.txt")); !os.IsNotExist(err) {
		t.Fatal("worker edit changed the live source worktree")
	}
	job := jobID(t, string(output))
	patch, err := os.ReadFile(filepath.Join(home, "runs", job+".patch"))
	if err != nil || !strings.Contains(string(patch), "+container edit") || strings.Contains(string(patch), "PRIVATE_SENTINEL") {
		t.Fatalf("container edit was not returned as a private patch: %v", err)
	}
	if entries, err := exec.Command("docker", "ps", "--quiet", "--filter", "label=switchyard.job="+job).Output(); err != nil || strings.TrimSpace(string(entries)) != "" {
		t.Fatal("completed job left its container running")
	}
}

func TestRootlessWorkerArgumentsDoNotExposeInvocationValues(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("SWITCHYARD_WORKER_NETWORK", "bridge")
	root := filepath.Join(t.TempDir(), "worker")
	jobPath := filepath.Join(root, "projects", strings.Repeat("a", 32), "jobs", strings.Repeat("b", 32))
	checkout := filepath.Join(jobPath, "worktree")
	if err := privateTree(checkout); err != nil {
		t.Fatal(err)
	}
	secret := "private-value-with-newline\nnext-line"
	cmd, setup, err := containerJobCommand(root, jobPath, checkout, ".", []string{"node", "-v"},
		[]string{"HOME=/from-invoker", "XDG_DATA_HOME=/from-invoker/data", "USER=switchyard-invoker", "LOGNAME=switchyard-invoker", "SWITCHYARD_TEST_SECRET=" + secret, "npm_config_store_dir=/workspace/.pnpm-store"},
		workerImage{ID: "sha256:" + strings.Repeat("c", 64), Runtime: "podman"})
	if err != nil || setup {
		t.Fatalf("rootless job could not be prepared: %v", err)
	}
	arguments := strings.Join(cmd.Args, " ")
	if strings.Contains(arguments, secret) || strings.Contains(arguments, "switchyard-invoker") || !strings.Contains(arguments, "--env-file /proc/self/fd/3") || !strings.Contains(arguments, "--userns=keep-id") ||
		!strings.Contains(arguments, "--network=pasta") || strings.Contains(arguments, "/from-invoker") ||
		!strings.Contains(arguments, "--env npm_config_store_dir=/home/switchyard/.local/share/pnpm/store") || strings.Contains(arguments, "/workspace/.pnpm-store") {
		t.Fatal("rootless job changed namespace or exposed invocation values")
	}
	if !strings.Contains(strings.Join(cmd.Env, "\n"), "SWITCHYARD_TEST_SECRET="+secret) || strings.Contains(strings.Join(cmd.Env, "\n"), "switchyard-invoker") ||
		strings.Contains(strings.Join(cmd.Env, "\n"), "/from-invoker") || strings.Contains(strings.Join(cmd.Env, "\n"), "/workspace/.pnpm-store") {
		t.Fatal("container engine received an incorrect invocation environment")
	}
	if len(cmd.ExtraFiles) != 1 {
		t.Fatal("container identity was not handed to the engine through a private fd")
	}
	defer cmd.ExtraFiles[0].Close()
	if _, err := os.Stat(cmd.ExtraFiles[0].Name()); !os.IsNotExist(err) {
		t.Fatal("container identity remained in a named file")
	}
	identity := make([]byte, 256)
	count, err := cmd.ExtraFiles[0].Read(identity)
	if err != nil || string(identity[:count]) != "USER=switchyard-invoker\nLOGNAME=switchyard-invoker\n" {
		t.Fatal("container identity was not preserved")
	}
	if err := os.Remove(filepath.Join(bin, "podman")); err != nil {
		t.Fatal(err)
	}
	stopJobContainer(jobPath, strings.Repeat("a", 32), strings.Repeat("b", 32))
}

func TestPinnedNodeImageInstallsLockedDependencies(t *testing.T) {
	image := os.Getenv("SWITCHYARD_TEST_NODE_IMAGE")
	if image == "" {
		t.Skip("set SWITCHYARD_TEST_NODE_IMAGE to the locally built pinned image ID")
	}
	if parent := os.Getenv("SWITCHYARD_TEST_DOCKER_PARENT"); parent != "" {
		t.Setenv("TMPDIR", parent)
	} else {
		t.Skip("set SWITCHYARD_TEST_DOCKER_PARENT to a directory visible to the Docker daemon")
	}
	root, home, exe := fixture(t), t.TempDir(), binary(t)
	for name, content := range map[string]string{
		"package.json":   `{"name":"switchyard-pinned-image-test","version":"1.0.0","packageManager":"pnpm@10.28.0","private":true,"scripts":{"test":"node -e \"require('fs').writeFileSync('agent-note.txt', require('child_process').execFileSync('pnpm', ['store', 'path'], {encoding:'utf8'}).trim())\""}}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\nimporters:\n  .: {}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := selectFiles(root, nil)
	if err != nil || !equalPaths(selected.Paths, []string{".gitignore", "README.md", "new.txt", "package.json", "pnpm-lock.yaml"}) || !equalPaths(selected.Excluded, []string{".env"}) {
		t.Fatalf("unexpected Node job preview: %+v %v", selected, err)
	}
	if err := initWorker(filepath.Join(home, "worker")); err != nil {
		t.Fatal(err)
	}
	if err := saveImage(filepath.Join(home, "worker"), image); err != nil {
		t.Fatal(err)
	}
	t.Setenv("npm_config_store_dir", "/workspace/.pnpm-store")
	status, output := command(t, exe, home, "run", "--backend", "local", "--repo", root, "--", "pnpm", "test")
	if status != 0 || !strings.Contains(output, "switchyard-pinned-image-test") {
		t.Fatalf("locked pnpm job failed: exit=%d output=%s", status, output)
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules")); !os.IsNotExist(err) {
		t.Fatal("dependency setup changed the source worktree")
	}
	job := jobID(t, output)
	patch, err := os.ReadFile(filepath.Join(home, "runs", job+".patch"))
	if err != nil || !strings.Contains(string(patch), "+/home/switchyard/.local/share/pnpm/store") || strings.Contains(string(patch), "node_modules") || strings.Contains(string(patch), ".pnpm-store") {
		t.Fatalf("pnpm edits did not remain isolated: %v", err)
	}
}
