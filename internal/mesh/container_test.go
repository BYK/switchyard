package mesh

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestContainerCWDRejectsBroadOrMissingMount(t *testing.T) {
	if _, err := containerCWD("/"); err == nil {
		t.Fatal("sandbox must never bind mount the entire host root")
	}
	if _, err := containerCWD(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("sandbox must not create an unintended host mount source")
	}
	if _, err := containerCWD(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

func TestContainerCWDRejectsMountedDockerSocket(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	t.Setenv("DOCKER_HOST", "unix://"+socket)
	if _, err := containerCWD(root); err == nil || !strings.Contains(err.Error(), "container control socket") {
		t.Fatal("sandbox must reject a source that exposes its Docker daemon socket")
	}
}

// Run with MESH_TEST_CONTAINER_IMAGE set to a locally cached, digest-pinned
// image with /bin/bash (for example, docker.io/library/ubuntu@sha256:...).
func TestContainerShellSharesCWDAndOffersFreshIsolation(t *testing.T) {
	if parent := os.Getenv("MESH_TEST_DOCKER_PARENT"); parent != "" {
		t.Setenv("TMPDIR", parent)
	}
	image := os.Getenv("MESH_TEST_CONTAINER_IMAGE")
	if image == "" {
		t.Skip("set MESH_TEST_CONTAINER_IMAGE to run the Docker integration test")
	}
	if !containerImagePattern.MatchString(image) {
		t.Fatal("test image must be pinned by digest")
	}
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	entry := filepath.Join(t.TempDir(), "meshell")
	if err := os.Symlink(exe, entry); err != nil {
		t.Fatal(err)
	}
	baseEnv := append(os.Environ(), "MESH_GO_HOME="+home,
		"MESH_CONTAINER_IMAGE="+image, "MESH_CONTAINER_NETWORK=none",
		"MESH_CONTAINER_TEST=first\nsecond=ok")
	invoke := func(mode, command string, expected int) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, entry, "-c", command)
		cmd.Env = append(baseEnv, "MESH_CONTAINER_MODE="+mode)
		cmd.Dir = root
		output, err := cmd.CombinedOutput()
		if ctx.Err() != nil || exitStatus(err) != expected {
			t.Fatalf("%s container command exited %d, want %d; timed out=%t; output=%s",
				mode, exitStatus(err), expected, ctx.Err() != nil, output)
		}
		return strings.TrimSpace(string(output))
	}
	cleanup := func() {
		cmd := exec.Command(exe, "container", "stop", "--cwd", root)
		cmd.Env = append(os.Environ(), "MESH_GO_HOME="+home)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("could not remove test container: %s", output)
		}
	}
	t.Cleanup(cleanup)
	first := invoke("shared", `test "$MESH_CONTAINER_TEST" = $'first\nsecond=ok' && test "$PWD" = "$(pwd)" && test "$HOME" = /home/mesh && test ! -e /var/run/docker.sock && printf 'local edit\n' > live.txt && printf 'session\n' > "$HOME/marker" && cat /etc/hostname`, 0)
	if data, err := os.ReadFile(filepath.Join(root, "live.txt")); err != nil || string(data) != "local edit\n" {
		t.Fatal("local container did not use the writable live CWD mount")
	}
	second := invoke("shared", `test -f "$HOME/marker" && cat /etc/hostname`, 0)
	if first != second {
		t.Fatal("commands at the same CWD did not share one sandbox")
	}
	status := exec.Command(exe, "container", "status", "--cwd", root)
	status.Env = append(os.Environ(), "MESH_GO_HOME="+home)
	if output, err := status.CombinedOutput(); err != nil || !strings.Contains(string(output), "running") {
		t.Fatalf("sandbox is not reported running: %s", output)
	}
	changedProfile := exec.Command(entry, "-c", "exit 0")
	changedProfile.Dir = root
	changedProfile.Env = append(baseEnv, "MESH_CONTAINER_NETWORK=bridge")
	if output, err := changedProfile.CombinedOutput(); exitStatus(err) != 2 || !strings.Contains(string(output), "different image or settings") {
		t.Fatalf("CWD sandbox silently changed network profile: %s", output)
	}
	spoofed := exec.Command(entry, "-c", "test ! -e /var/run/docker.sock && cat /etc/hostname")
	spoofed.Dir = root
	spoofed.Env = append(baseEnv, "MESH_CONTAINER_SESSION=forged")
	if output, err := spoofed.CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != first {
		t.Fatalf("host environment bypassed the sandbox: %s", output)
	}
	// A service-style process must remain alive while another shell attaches.
	serviceCtx, cancelService := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelService()
	service := exec.CommandContext(serviceCtx, entry, "-c", `printf 'ready\n'; IFS= read -r message; test "$message" = finished`)
	service.Env = append(baseEnv, "MESH_CONTAINER_MODE=shared")
	service.Dir = root
	serviceInput, err := service.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	serviceOutput, err := service.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(serviceOutput).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("service did not start in the shared sandbox: %q", line)
		}
	case <-serviceCtx.Done():
		t.Fatal("service did not start")
	}
	if attached := invoke("shared", "cat /etc/hostname", 0); attached != first {
		t.Fatal("second shell could not attach while the service was running")
	}
	if _, err := serviceInput.Write([]byte("finished\n")); err != nil {
		t.Fatal(err)
	}
	if err := serviceInput.Close(); err != nil {
		t.Fatal(err)
	}
	if err := service.Wait(); err != nil {
		t.Fatalf("service did not stop cleanly: %v", err)
	}
	if got := invoke("shared", "exit 37", 37); got != "" {
		t.Fatalf("exit status changed or unexpected output: %s", got)
	}
	fresh := invoke("isolated", `test ! -e "$HOME/marker" && cat /etc/hostname`, 0)
	if fresh == second {
		t.Fatal("isolated invocation reused the shared container")
	}
	if got := invoke("shared", "id -u", 0); got != strconv.Itoa(os.Getuid()) {
		t.Fatalf("container did not use the invoking UID: %s", got)
	}
	if _, err := exec.LookPath("script"); err == nil {
		quote := func(value string) string {
			return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
		}
		ptyCtx, cancelPTY := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelPTY()
		pty := exec.CommandContext(ptyCtx, "script", "-q", "-e", "-c",
			quote(entry)+" -c "+quote("test -t 0 && test -t 1 && printf tty-ok"), "/dev/null")
		pty.Dir = root
		pty.Env = append(baseEnv, "MESH_CONTAINER_MODE=shared")
		if output, err := pty.CombinedOutput(); err != nil || !strings.Contains(string(output), "tty-ok") {
			t.Fatalf("interactive shell lost its terminal: %s; err=%v", output, err)
		}
	}
	stop := exec.Command(exe, "container", "stop", "--cwd", root)
	stop.Env = append(os.Environ(), "MESH_GO_HOME="+home)
	if output, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("could not prepare concurrent start: %s", output)
	}
	commands := make([]*exec.Cmd, 2)
	outputBuffers := make([]*bytes.Buffer, len(commands))
	stderrBuffers := make([]*bytes.Buffer, len(commands))
	concurrentCtx, cancelConcurrent := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelConcurrent()
	for i := range commands {
		commands[i] = exec.CommandContext(concurrentCtx, entry, "-c", "cat /etc/hostname")
		commands[i].Dir = root
		commands[i].Env = append(baseEnv, "MESH_CONTAINER_MODE=shared")
		outputBuffers[i] = &bytes.Buffer{}
		stderrBuffers[i] = &bytes.Buffer{}
		commands[i].Stdout = outputBuffers[i]
		commands[i].Stderr = stderrBuffers[i]
	}
	outputs := make([]string, len(commands))
	for _, command := range commands {
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		// Start both before waiting: neither process may assume it owns creation.
	}
	for i, command := range commands {
		if err := command.Wait(); err != nil {
			t.Fatalf("concurrent CWD start failed: %v; %s", err, stderrBuffers[i].String())
		}
		outputs[i] = strings.TrimSpace(outputBuffers[i].String())
	}
	if outputs[0] == "" || outputs[0] != outputs[1] {
		t.Fatal("concurrent shells did not attach to the same CWD sandbox")
	}
}
