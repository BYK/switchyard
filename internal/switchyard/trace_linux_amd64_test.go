package switchyard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTraceShellLeavesUnpreviewedCommandsUntraced(t *testing.T) {
	exe := binary(t)
	// An ordinary tool call must not install an inherited seccomp filter or
	// attach ptrace to its descendants just to learn local timings. Nested
	// debuggers and test tracers need to remain usable.
	script := `while IFS=: read -r label value; do
  if [ "$label" = TracerPid ]; then
    [ "$value" -eq 0 ]
    exit
  fi
done < /proc/self/status
exit 99`
	cmd := exec.Command(exe, "trace", "shell", "--", "-c", script)
	cmd.Env = append(os.Environ(), "SWITCHYARD_BACKEND=auto", "SWITCHYARD_TRACE_PREVIEWED=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unpreviewed command was traced: %v %q", err, output)
	}
}

func TestTraceShellRoutesOneExecAndPreservesLocalCommands(t *testing.T) {
	root := fixture(t)
	home := t.TempDir()
	exe := binary(t)
	if installed := os.Getenv("SWITCHYARD_TEST_INSTALLED_TRACE"); installed != "" {
		exe = installed
	}
	worker := filepath.Join(t.TempDir(), "worker")
	if err := saveBackend(home, backendConfig{Name: "sandbox", Kind: "local", Root: worker}); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"package.json":   `{"name":"trace-fixture","private":true}`,
		"pnpm-lock.yaml": "lockfileVersion: '9.0'\nimporters:\n  .: {}\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tool := filepath.Join(t.TempDir(), "pnpm")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nif [ \"$1\" = install ]; then mkdir node_modules; exit 0; fi\nif [ \"$1\" = --version ]; then\n  if [ \"$SWITCHYARD_TEST_REEXEC\" = 1 ] && [ \"$SWITCHYARD_TEST_CHILD\" != 1 ]; then SWITCHYARD_TEST_CHILD=1 \"$0\" \"$@\"; exit $?; fi\n  exit 19\nfi\ntest -d node_modules || exit 91\nprintf 'worker stdout\\n'\nprintf 'worker stderr\\n' >&2\nprintf '%s\\n' \"$SWITCHYARD_TRACE_INVOCATION\" \"$2\" > agent-note.txt\ncat >> agent-note.txt\nexit 37\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Dir(tool) + ":" + os.Getenv("PATH")
	checkPreview := func() {
		t.Helper()
		cmd := exec.Command(exe, "preview", "--repo", root)
		cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "PATH="+path)
		preview, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("preview failed: %v", err)
		}
		expected := "Snapshot: 5 files; 1 private paths excluded\n  + .gitignore\n  + README.md\n  + new.txt\n  + package.json\n  + pnpm-lock.yaml\n  - .env (excluded)\n  outputs: discover generated ignored paths automatically\n"
		if _, err := os.Stat(filepath.Join(root, "agent-note.txt")); err == nil {
			expected = strings.Replace(expected, "Snapshot: 5", "Snapshot: 6", 1)
			expected = strings.Replace(expected, "  + README.md\n", "  + README.md\n  + agent-note.txt\n", 1)
		}
		if string(preview) != expected {
			t.Fatalf("preview changed its selected or excluded paths: %s", preview)
		}
	}
	run := func(backend, script, stdin string, extraEnv ...string) (int, string, string) {
		t.Helper()
		checkPreview()
		cmd := exec.Command(exe, "trace", "shell", "--", "-c", script)
		cmd.Dir = root
		cmd.Env = append(append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_BACKEND="+backend, "PATH="+path, "SWITCHYARD_TRACE_INVOCATION=late value"), extraEnv...)
		cmd.Stdin = strings.NewReader(stdin)
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil {
			return 0, stdout.String(), stderr.String()
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), stdout.String(), stderr.String()
		}
		t.Fatalf("traced shell could not start: %v", err)
		return 2, "", ""
	}
	status, stdout, stderr := run("sandbox", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm test 'quoted value'", "trace stdin")
	if status != 37 || !strings.Contains(stdout, "worker stdout\n") || !strings.Contains(stderr, "worker stderr\n") {
		t.Fatalf("exec was not routed with its status and streams: %d %q %q", status, stdout, stderr)
	}
	if content, err := os.ReadFile(filepath.Join(root, "agent-note.txt")); err != nil || !strings.Contains(string(content), "trace stdin") {
		t.Fatal("traced job did not return worker edits")
	}
	patches, err := filepath.Glob(filepath.Join(home, "runs", "*.patch"))
	if err != nil || len(patches) != 1 {
		t.Fatalf("traced job did not return exactly one patch: %v %v", patches, err)
	}
	patch, err := os.ReadFile(patches[0])
	if err != nil || !strings.Contains(string(patch), "+late value") || !strings.Contains(string(patch), "+quoted value") || !strings.Contains(string(patch), "+trace stdin") || strings.Contains(string(patch), ".env") {
		t.Fatalf("unexpected retained patch: %v", err)
	}
	// Bash must remain alive to run an EXIT trap, so it forks even this single
	// foreground command. The routed child must keep Bash's trap and exit status.
	bashEnv := filepath.Join(t.TempDir(), "bash-env")
	if err := os.WriteFile(bashEnv, []byte("trap 'printf \"shell-exit-trap\\n\"' EXIT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, stdout, stderr = run("sandbox", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm test 'quoted value'", "traced child stdin", "BASH_ENV="+bashEnv)
	if status != 37 || !strings.Contains(stdout, "worker stdout\n") || !strings.Contains(stdout, "shell-exit-trap\n") || !strings.Contains(stderr, "worker stderr\n") {
		t.Fatalf("foreground Bash child was not routed while preserving the EXIT trap: %d %q %q", status, stdout, stderr)
	}
	patches, err = filepath.Glob(filepath.Join(home, "runs", "*.patch"))
	if err != nil || len(patches) != 2 {
		t.Fatalf("foreground child did not produce an isolated patch: %v %v", patches, err)
	}
	status, _, _ = run("auto", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm --version", "")
	if status != 19 {
		t.Fatalf("unpreviewed automatic command was redirected: %d", status)
	}
	key, ok := commandSignature(root, tool, []string{"--version"})
	if !ok {
		t.Fatal("local command observation could not be identified")
	}
	history, err := readObservations(home)
	if err != nil || len(history[key].Backends["local"]) == 0 {
		t.Fatalf("traced local command timing was not learned: %v", err)
	}
	status, stdout, _ = run("auto", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm --version", "", "BASH_ENV="+bashEnv)
	if status != 19 || !strings.Contains(stdout, "shell-exit-trap\n") {
		t.Fatalf("foreground local command did not preserve Bash's exit trap: %d %q", status, stdout)
	}
	history, err = readObservations(home)
	if err != nil || len(history[key].Backends["local"]) != 2 {
		t.Fatalf("forked local command timing was not learned: %v", err)
	}
	status, _, _ = run("auto", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm --version", "", "SWITCHYARD_TEST_REEXEC=1")
	if status != 19 {
		t.Fatalf("a tool launching another executable changed its exit status: %d", status)
	}
	history, err = readObservations(home)
	if err != nil || len(history[key].Backends["local"]) != 3 {
		t.Fatalf("a single shell command produced more than one local timing sample: %v, %d", err, len(history[key].Backends["local"]))
	}
	status, _, _ = run("sandbox", "pnpm --version", "")
	if status != 2 {
		t.Fatalf("unpreviewed forced command did not fail closed: %d", status)
	}
	status, stdout, stderr = run("sandbox", "SWITCHYARD_TRACE_PREVIEWED=1 python3 -c 'import sys; sys.exit(29)'", "")
	if status != 29 {
		t.Fatalf("an unlisted external command did not preserve its exit: %d %q %q", status, stdout, stderr)
	}
	status, stdout, stderr = run("sandbox", "printf 'unrelated' | tr a-z A-Z", "")
	if status != 0 || stdout != "UNRELATED" || stderr != "" {
		t.Fatalf("shell pipeline changed: %d %q %q", status, stdout, stderr)
	}
	status, _, _ = run("sandbox", "pnpm test | cat", "")
	if status != 0 {
		t.Fatalf("eligible tool inside a pipeline changed the shell status: %d", status)
	}
	status, _, _ = run("sandbox", "pnpm test > /dev/null", "")
	if status != 91 {
		t.Fatalf("eligible tool with redirection left Bash: %d", status)
	}
	patches, err = filepath.Glob(filepath.Join(home, "runs", "*.patch"))
	if err != nil || len(patches) != 2 {
		t.Fatal("pipeline or redirection ran on the worker")
	}
	status, _, _ = run("sandbox", "pnpm test", "")
	if status != 2 {
		t.Fatalf("forced backend without a per-invocation preview did not fail closed: %d", status)
	}
	status, _, _ = run("local", "pnpm test", "")
	if status != 91 {
		t.Fatalf("local override did not run the original tool: %d", status)
	}
	status, _, _ = run("auto", "pnpm test", "")
	if status != 91 {
		t.Fatalf("automatic local fallback intercepted itself: %d", status)
	}
	status, _, _ = run("sandbox", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm test | cat", "")
	if status != 0 {
		t.Fatalf("preview marker caused a pipeline to offload: %d", status)
	}
	status, _, _ = run("sandbox", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm test > /dev/null", "")
	if status != 91 {
		t.Fatalf("preview marker caused a redirected command to offload: %d", status)
	}
	status, _, _ = run("sandbox", "pnpm test; printf done", "")
	if status != 0 {
		t.Fatalf("tool in a compound command was diverted from Bash: %d", status)
	}
	status, _, _ = run("sandbox", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm test; printf done", "")
	if status != 0 {
		t.Fatalf("previewed compound command was diverted from Bash: %d", status)
	}
	status, _, _ = run("sandbox", "SWITCHYARD_TRACE_PREVIEWED=1 pnpm test; pnpm --version", "")
	if status != 19 {
		t.Fatalf("final eligible command in a compound script was diverted from Bash: %d", status)
	}
	patches, err = filepath.Glob(filepath.Join(home, "runs", "*.patch"))
	if err != nil || len(patches) != 2 {
		t.Fatal("a command with different shell semantics ran on the worker")
	}
	alternate := filepath.Join(t.TempDir(), "pnpm")
	if err := os.WriteFile(alternate, []byte("#!/bin/sh\nexit 19\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	hashEnv := filepath.Join(t.TempDir(), "bash-env")
	if err := os.WriteFile(hashEnv, []byte("hash -p "+strconv.Quote(alternate)+" pnpm\ntrap ':' EXIT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mismatch := "SWITCHYARD_TRACE_PREVIEWED=1 pnpm test"
	status, _, _ = run("sandbox", mismatch, "", "BASH_ENV="+hashEnv)
	if status != 2 {
		t.Fatalf("forced backend ran a Bash-hashed executable other than Switchyard's PATH result: %d", status)
	}
	status, _, _ = run("auto", mismatch, "", "BASH_ENV="+hashEnv)
	if status != 19 {
		t.Fatalf("automatic placement changed Bash's hashed executable: %d", status)
	}
}

func TestTraceInstallRejectsForeignLinkAndRepeatsSafely(t *testing.T) {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	directory := filepath.Join(home, "ptrace", "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(bin, "switchyard")
	link := filepath.Join(directory, "bash")
	if _, err := installTraceShell(home, current); err != nil {
		t.Fatalf("could not install the private traced-shell link: %v", err)
	}
	if target, err := os.Readlink(link); err != nil || target != current {
		t.Fatalf("traced-shell link points to %q: %v", target, err)
	}
	if _, err := installTraceShell(home, current); err != nil {
		t.Fatalf("repeat install failed: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/true", link); err != nil {
		t.Fatal(err)
	}
	if _, err := installTraceShell(home, current); err == nil {
		t.Fatal("replaced an unrelated traced-shell link")
	}
}

func TestTraceShellNameSelectsTracerAndInteractivePassesThrough(t *testing.T) {
	exe := binary(t)
	home := t.TempDir()
	install := exec.Command(exe, "trace", "install")
	install.Env = append(os.Environ(), "SWITCHYARD_HOME="+home)
	output, err := install.CombinedOutput()
	if err != nil {
		t.Fatalf("trace install failed: %v %q", err, output)
	}
	shell := strings.TrimSpace(string(output))
	if shell != filepath.Join(home, "ptrace", "bin", "bash") {
		t.Fatalf("trace install returned an unexpected shell: %q", shell)
	}
	cmd := exec.Command(shell, "-c", "printf 'shell tool'")
	cmd.Env = append(os.Environ(), "SWITCHYARD_TRACE_SHELL=1", "SWITCHYARD_BACKEND=auto", "SWITCHYARD_HOME="+t.TempDir())
	if output, err := cmd.CombinedOutput(); err != nil || string(output) != "shell tool" {
		t.Fatalf("OpenCode-style shell did not start: %v %q", err, output)
	}
	cmd = exec.Command(shell, "-i", "-c", "printf 'terminal'")
	cmd.Env = append(os.Environ(), "SWITCHYARD_TRACE_SHELL=1", "SWITCHYARD_BACKEND=auto", "SWITCHYARD_HOME="+t.TempDir())
	output, err = cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "terminal") {
		t.Fatalf("interactive shell did not pass through to Bash: %v %q", err, output)
	}
	if script, scriptErr := exec.LookPath("script"); scriptErr == nil {
		bin := t.TempDir()
		tool := filepath.Join(bin, "pnpm")
		if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 19\n"), 0o700); err != nil {
			t.Fatal(err)
		}
		terminal := exec.Command(script, "-q", "-e", "-c", shell+" -c 'pnpm --version'", "/dev/null")
		terminal.Env = append(os.Environ(), "SHELL=/usr/bin/bash", "PATH="+bin+":"+os.Getenv("PATH"), "SWITCHYARD_TRACE_SHELL=1", "SWITCHYARD_BACKEND=sandbox", "SWITCHYARD_HOME="+home)
		if output, err := terminal.CombinedOutput(); err == nil || err.(*exec.ExitError).ExitCode() != 19 {
			t.Fatalf("terminal -c was unexpectedly traced: %v %q", err, output)
		}
	}
}

func TestTraceShellReturnsWithoutKillingBackgroundCommands(t *testing.T) {
	root := t.TempDir()
	exe := binary(t)
	input := filepath.Join(root, "input.fifo")
	done := filepath.Join(root, "done.fifo")
	for _, path := range []string{input, done} {
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inputFile, err := os.OpenFile(input, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer inputFile.Close()
	doneFile, err := os.OpenFile(done, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer doneFile.Close()
	// The background command waits until after Bash exits. An inherited seccomp
	// filter must still have a tracer when that command next uses execve.
	script := "(read line < " + input + "; /usr/bin/printf x > " + done + ") </dev/null >/dev/null 2>&1 &"
	cmd := exec.Command(exe, "trace", "shell", "--", "-c", script)
	cmd.Env = append(os.Environ(), "SWITCHYARD_BACKEND=auto", "SWITCHYARD_HOME="+t.TempDir())
	returned := make(chan error, 1)
	go func() { returned <- cmd.Run() }()
	select {
	case err := <-returned:
		if err != nil {
			t.Fatalf("Bash did not return its status: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("tracer held the foreground command open for a background child")
	}
	if _, err := inputFile.Write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	complete := make(chan error, 1)
	go func() {
		var result [1]byte
		_, err := doneFile.Read(result[:])
		if err == nil && result[0] != 'x' {
			err = syscall.EIO
		}
		complete <- err
	}()
	select {
	case err := <-complete:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the traced background child was killed or lost its exec")
	}
}

func TestTraceShellKilledWrapperReapsTracee(t *testing.T) {
	exe := binary(t)
	cmd := exec.Command(exe, "trace", "shell", "--", "-c", "printf '%s\\n' $$; exec /usr/bin/sleep 30")
	cmd.Env = append(os.Environ(), "SWITCHYARD_BACKEND=auto", "SWITCHYARD_HOME="+t.TempDir())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var line [32]byte
	count := 0
	for count < len(line) {
		n, err := stdout.Read(line[count : count+1])
		if err != nil || n != 1 {
			_ = cmd.Process.Kill()
			t.Fatal("traced Bash did not start")
		}
		if line[count] == '\n' {
			break
		}
		count++
	}
	pid, err := strconv.Atoi(string(line[:count]))
	if err != nil || pid <= 0 {
		_ = cmd.Process.Kill()
		t.Fatal("could not identify traced shell")
	}
	const pidfdOpen = 434 // x86-64 Linux syscall
	fd, _, errno := syscall.RawSyscall(pidfdOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		_ = cmd.Process.Kill()
		t.Fatalf("could not track child lifetime: %v", errno)
	}
	defer syscall.Close(int(fd))
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if fd >= 1024 {
		t.Fatal("pidfd exceeds select FD_SETSIZE")
	}
	var readable syscall.FdSet
	readable.Bits[fd/64] = 1 << (fd % 64)
	timeout := syscall.NsecToTimeval((5 * time.Second).Nanoseconds())
	ready, err := syscall.Select(int(fd+1), &readable, nil, nil, &timeout)
	if err != nil || ready != 1 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("traced shell survived a killed wrapper: ready=%d error=%v", ready, err)
	}
}

func TestTraceShellReturnsWhileStdinRemainsOpen(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	cmd := exec.Command(binary(t), "trace", "shell", "--", "-c", "/usr/bin/true")
	cmd.Env = append(os.Environ(), "SWITCHYARD_BACKEND=auto", "SWITCHYARD_HOME="+t.TempDir())
	cmd.Stdin = input
	complete := make(chan error, 1)
	go func() { complete <- cmd.Run() }()
	select {
	case err := <-complete:
		if err != nil {
			t.Fatalf("shell command failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("trace waited for terminal stdin to close after Bash exited")
	}
}
