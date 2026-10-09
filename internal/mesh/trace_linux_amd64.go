package mesh

import (
	binaryle "encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	traceFilter         = 0x7ff00000
	traceAllow          = 0x7fff0000
	traceEventSeccomp   = 7
	traceOptions        = 0x00000002 | 0x00000004 | 0x00000008 | 0x00000010 | 0x00000080 | 0x00100000
	traceAllChildren    = 0x40000000
	traceBPFLoadWord    = 0x20
	traceBPFEqual       = 0x15
	traceBPFReturn      = 0x06
	traceArchAMD64      = 0xc000003e
	traceMaxString      = 4096
	traceMaxEnvironment = 4096
	traceExecveat       = 322 // /usr/include/x86_64-linux-gnu/asm/unistd_64.h
)

func traceCommand(home string, self string, args []string) int {
	if len(args) == 1 && args[0] == "install" {
		shell, err := installTraceShell(home, self)
		if err != nil {
			return fail(err)
		}
		_, _ = os.Stdout.WriteString(shell + "\n")
		return 0
	}
	if len(args) > 0 && args[0] == "child" {
		return traceChild(self, args[1:])
	}
	if len(args) > 0 && args[0] == "supervise" && os.Getenv("MESH_TRACE_SUPERVISOR") == "1" {
		return traceSupervisor(self, args[1:])
	}
	if len(args) >= 1 && args[0] == "shell" {
		args = args[1:]
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		return traceShell(self, args)
	}
	return fail(errors.New("use trace shell -- BASH_ARGS"))
}

func installTraceShell(home string, self string) (string, error) {
	parent := filepath.Join(home, "ptrace")
	if err := privateTree(parent); err != nil {
		return "", err
	}
	directory := filepath.Join(parent, "bin")
	if err := privateTree(directory); err != nil {
		return "", err
	}
	shell := filepath.Join(directory, "bash")
	if info, err := os.Lstat(shell); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return "", errors.New("traced shell path already contains an unrelated file")
		}
		target, err := os.Readlink(shell)
		if err != nil {
			return "", errors.New("traced shell path already points elsewhere")
		}
		if target == self {
			return shell, nil
		}
		// Migrate only the private, unpublished CLI installed beside this binary.
		old := filepath.Join(filepath.Dir(self), "mesh-go")
		oldInfo, oldErr := os.Lstat(old)
		if target != old || oldErr != nil || !oldInfo.Mode().IsRegular() || oldInfo.Mode().Perm()&0o111 == 0 {
			return "", errors.New("traced shell path already points elsewhere")
		}
		replacement := filepath.Join(directory, "bash.switchyard-next")
		if err := os.Symlink(self, replacement); err != nil {
			return "", errors.New("could not prepare traced shell migration")
		}
		defer os.Remove(replacement)
		if err := os.Rename(replacement, shell); err != nil {
			return "", errors.New("could not migrate traced shell link")
		}
		return shell, nil
	} else if !os.IsNotExist(err) {
		return "", errors.New("could not inspect traced shell path")
	}
	if err := os.Symlink(self, shell); err != nil {
		return "", errors.New("could not install traced shell link")
	}
	return shell, nil
}

func traceChild(self string, args []string) int {
	if os.Getenv("MESH_TRACE_CHILD") != "1" {
		return fail(errors.New("trace child requires a supervisor"))
	}
	runtime.LockOSThread()
	filter := []syscall.SockFilter{
		{Code: traceBPFLoadWord, K: 4},
		{Code: traceBPFEqual, Jf: 3, K: traceArchAMD64},
		{Code: traceBPFLoadWord, K: 0},
		{Code: traceBPFEqual, Jt: 2, K: syscall.SYS_EXECVE},
		{Code: traceBPFEqual, Jt: 1, K: traceExecveat},
		{Code: traceBPFReturn, K: traceAllow},
		{Code: traceBPFReturn, K: traceFilter},
	}
	program := syscall.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 1, 0, 0, 0, 0)
	if errno != 0 {
		return fail(errors.New("traced child could not set no-new-privileges"))
	}
	_, _, errno = syscall.RawSyscall6(syscall.SYS_PRCTL, 22, 2, uintptr(unsafe.Pointer(&program)), 0, 0, 0)
	if errno != 0 {
		return fail(errors.New("traced child could not install exec filter"))
	}
	environment := os.Environ()
	if err := syscall.Exec("/usr/bin/bash", append([]string{"bash"}, args...), environment); err != nil {
		return fail(errors.New("traced shell could not start"))
	}
	return 0
}

func traceShell(self string, args []string) int {
	commandString := false
	for index, arg := range args {
		if arg == "-i" || arg == "--interactive" {
			commandString = false
			break
		}
		if arg == "-c" && index+1 < len(args) {
			commandString = true
		}
	}
	if os.Getenv("MESH_BACKEND") == "local" || !commandString || isTTY(os.Stdin) {
		if err := syscall.Exec("/usr/bin/bash", append([]string{"bash"}, args...), os.Environ()); err != nil {
			return fail(errors.New("could not run local Bash"))
		}
		return 0
	}
	// The foreground wrapper returns as soon as Bash exits. Its supervisor
	// stays alive for background children, which inherit the seccomp filter.
	reader, writer, err := os.Pipe()
	if err != nil {
		return fail(errors.New("could not open traced shell status channel"))
	}
	command := exec.Command(self, append([]string{"trace", "supervise"}, args...)...)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	command.ExtraFiles = []*os.File{writer}
	command.Env = append(os.Environ(), "MESH_TRACE_SUPERVISOR=1")
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := command.Start(); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return fail(errors.New("could not start exec supervisor"))
	}
	_ = writer.Close()
	var status [4]byte
	_, err = io.ReadFull(reader, status[:])
	_ = reader.Close()
	_ = command.Process.Release()
	if err != nil || binaryle.LittleEndian.Uint32(status[:]) > 255 {
		return fail(errors.New("exec supervisor did not report a valid command status"))
	}
	return int(binaryle.LittleEndian.Uint32(status[:]))
}

func traceSupervisor(self string, args []string) int {
	statusFile := os.NewFile(3, "traced-shell-status")
	if statusFile == nil {
		return fail(errors.New("exec supervisor needs a status channel"))
	}
	defer statusFile.Close()
	reported := false
	defer func() {
		if !reported {
			var status [4]byte
			binaryle.LittleEndian.PutUint32(status[:], 2)
			_, _ = statusFile.Write(status[:])
		}
	}()
	// Ptrace operations must all run from the thread that started the tracee.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	child := exec.Command(self, append([]string{"trace", "child"}, args...)...)
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Env = append(os.Environ(), "MESH_TRACE_CHILD=1", "MESH_TRACE_HANDOFF=0", "MESH_TRACE_PROXY_PATH="+self, "SHELL=/usr/bin/bash")
	child.SysProcAttr = &syscall.SysProcAttr{Ptrace: true}
	if err := child.Start(); err != nil {
		return fail(errors.New("could not start traced Bash"))
	}
	root := child.Process.Pid
	defer child.Process.Release()
	live := map[int]bool{root: true}
	rootExecSeen := false
	localSignature := ""
	localStarted := time.Time{}
	standardFiles := make([]os.FileInfo, 3)
	for index, file := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		info, err := file.Stat()
		if err != nil {
			return fail(errors.New("could not inspect shell file descriptors"))
		}
		standardFiles[index] = info
	}
	for len(live) > 0 {
		var status syscall.WaitStatus
		var usage syscall.Rusage
		pid, err := syscall.Wait4(-1, &status, traceAllChildren, &usage)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return fail(errors.New("could not wait for traced command"))
		}
		if status.Exited() || status.Signaled() {
			delete(live, pid)
			if pid == root {
				if status.Exited() && localSignature != "" && !localStarted.IsZero() {
					if home, err := dataHome(); err == nil {
						cpu := time.Duration(usage.Utime.Sec+usage.Stime.Sec)*time.Second + time.Duration(usage.Utime.Usec+usage.Stime.Usec)*time.Microsecond
						_ = recordCommandObservation(home, localSignature, "local", time.Since(localStarted), cpu)
					}
				}
				code := 128 + int(status.Signal())
				if status.Exited() {
					code = status.ExitStatus()
				}
				// The wrapper may now exit, but background jobs still need this tracer.
				_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 1, 0, 0, 0, 0, 0)
				if errno != 0 {
					return fail(errors.New("could not detach exec supervisor from foreground wrapper"))
				}
				var message [4]byte
				binaryle.LittleEndian.PutUint32(message[:], uint32(code))
				if _, err := statusFile.Write(message[:]); err != nil {
					return fail(errors.New("could not report traced shell result"))
				}
				reported = true
				_ = statusFile.Close()
				// Do not hold OpenCode's output pipes open for surviving background jobs.
				_ = os.Stdin.Close()
				_ = os.Stdout.Close()
				_ = os.Stderr.Close()
			}
			continue
		}
		if !status.Stopped() {
			continue
		}
		if err := syscall.PtraceSetOptions(pid, traceOptions); err != nil {
			return fail(errors.New("could not configure exec tracing"))
		}
		event := status.TrapCause()
		if event == 1 || event == 2 || event == 3 {
			child, err := syscall.PtraceGetEventMsg(pid)
			if err != nil {
				return fail(errors.New("could not follow spawned process"))
			}
			live[int(child)] = true
		}
		if event == traceEventSeccomp && pid == root {
			if !rootExecSeen {
				// The first filtered exec starts Bash itself, before it parses -c.
				rootExecSeen = true
			} else {
				localSignature, localStarted = traceSignature(pid), time.Now()
				if traceRedirect(pid, self, standardFiles) {
					localSignature = ""
				}
			}
		}
		signal := 0
		if status.StopSignal() != syscall.SIGTRAP && status.StopSignal() != syscall.SIGSTOP {
			signal = int(status.StopSignal())
		}
		if err := syscall.PtraceCont(pid, signal); err != nil && err != syscall.ESRCH {
			return fail(errors.New("could not resume traced process"))
		}
	}
	return 0
}

func traceString(pid int, addr uint64, limit int) (string, bool) {
	if addr == 0 || limit < 1 {
		return "", false
	}
	text := make([]byte, limit)
	for offset := 0; offset < limit; offset += 8 {
		length := min(8, limit-offset)
		if _, err := syscall.PtracePeekData(pid, uintptr(addr+uint64(offset)), text[offset:offset+length]); err != nil {
			return "", false
		}
		if index := strings.IndexByte(string(text[offset:offset+length]), 0); index >= 0 {
			return string(text[:offset+index]), true
		}
	}
	return "", false
}

func tracePointer(pid int, addr uint64) (uint64, bool) {
	var data [8]byte
	if _, err := syscall.PtracePeekData(pid, uintptr(addr), data[:]); err != nil {
		return 0, false
	}
	return binaryle.LittleEndian.Uint64(data[:]), true
}

func traceSignature(pid int) string {
	var regs syscall.PtraceRegs
	if syscall.PtraceGetRegs(pid, &regs) != nil || regs.Orig_rax != syscall.SYS_EXECVE {
		return ""
	}
	path, ok := traceString(pid, regs.Rdi, traceMaxString)
	if !ok || !filepath.IsAbs(path) {
		return ""
	}
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd")
	if err != nil {
		return ""
	}
	args := make([]string, 0, 8)
	for index := 1; index <= 256; index++ {
		pointer, ok := tracePointer(pid, regs.Rsi+uint64(index*8))
		if !ok {
			return ""
		}
		if pointer == 0 {
			key, valid := commandSignature(cwd, path, args)
			if valid {
				return key
			}
			return ""
		}
		arg, ok := traceString(pid, pointer, traceMaxString)
		if !ok {
			return ""
		}
		args = append(args, arg)
	}
	return ""
}

func traceRedirect(pid int, self string, standardFiles []os.FileInfo) bool {
	var regs syscall.PtraceRegs
	if syscall.PtraceGetRegs(pid, &regs) != nil {
		return false
	}
	for index, expected := range standardFiles {
		actual, err := os.Stat("/proc/" + strconv.Itoa(pid) + "/fd/" + strconv.Itoa(index))
		if err != nil || !os.SameFile(expected, actual) {
			// Redirections, pipelines, and changed stdin stay with Bash.
			return false
		}
	}
	var pathPtr, argvPtr, envPtr uint64
	switch regs.Orig_rax {
	case syscall.SYS_EXECVE:
		pathPtr, argvPtr, envPtr = regs.Rdi, regs.Rsi, regs.Rdx
	case traceExecveat:
		// Relative dirfd paths and AT_EMPTY_PATH need their own path semantics.
		return false
	default:
		return false
	}
	path, ok := traceString(pid, pathPtr, traceMaxString)
	if !ok || !filepath.IsAbs(path) || path == self {
		return false
	}
	if selected, selectErr := os.Stat(path); selectErr == nil {
		if supervisor, selfErr := os.Stat(self); selfErr == nil && os.SameFile(selected, supervisor) {
			return false
		}
	}
	tool := filepath.Base(path)
	argv0, ok := tracePointer(pid, argvPtr)
	if !ok {
		return false
	}
	first, ok := traceString(pid, argv0, traceMaxString)
	if !ok || first != tool && filepath.Base(first) != tool {
		return false
	}
	var proxyAddress uint64
	var handoffAddress uint64
	pathValue := ""
	backend := "auto"
	bypass := false
	previewed := false
	pathSet := false
	environmentComplete := false
	seen := make(map[string]bool, 6)
	for i := 0; i < traceMaxEnvironment; i++ {
		ptr, ok := tracePointer(pid, envPtr+uint64(i*8))
		if !ok {
			return false
		}
		if ptr == 0 {
			environmentComplete = true
			break
		}
		value, ok := traceString(pid, ptr, traceMaxString)
		if !ok {
			return false
		}
		name, _, _ := strings.Cut(value, "=")
		if name == "PATH" || name == "MESH_BACKEND" || name == "MESH_TRACE_PROXY_PATH" || name == "MESH_TRACE_HANDOFF" || name == "MESH_TRACE_BYPASS" || name == "MESH_TRACE_PREVIEWED" {
			if seen[name] {
				return false
			}
			seen[name] = true
		}
		switch {
		case strings.HasPrefix(value, "MESH_TRACE_PROXY_PATH="):
			proxyAddress = ptr + uint64(len("MESH_TRACE_PROXY_PATH="))
		case value == "MESH_TRACE_HANDOFF=0":
			handoffAddress = ptr + uint64(len("MESH_TRACE_HANDOFF="))
		case strings.HasPrefix(value, "MESH_BACKEND="):
			backend = strings.TrimPrefix(value, "MESH_BACKEND=")
		case value == "MESH_TRACE_BYPASS=1":
			bypass = true
		case value == "MESH_TRACE_PREVIEWED=1":
			previewed = true
		case strings.HasPrefix(value, "PATH="):
			pathValue, pathSet = strings.TrimPrefix(value, "PATH="), true
		}
	}
	if !environmentComplete || backend == "local" || bypass || backend == "auto" && !previewed || !pathSet || proxyAddress == 0 || handoffAddress == 0 || !filepath.IsAbs(self) {
		return false
	}
	// The requested executable must be the same inode Mesh would find on PATH.
	selected, err := os.Stat(path)
	if err != nil || !selected.Mode().IsRegular() || selected.Mode().Perm()&0o111 == 0 {
		return false
	}
	match := false
	for _, directory := range filepath.SplitList(pathValue) {
		if !filepath.IsAbs(directory) {
			return false
		}
		candidate, err := os.Stat(filepath.Join(directory, tool))
		if err == nil && candidate.Mode().IsRegular() && candidate.Mode().Perm()&0o111 != 0 {
			match = os.SameFile(selected, candidate)
			break
		}
	}
	if !match && backend == "auto" {
		return false
	}
	// Keep Bash's resolved path in argv[0] while replacing only the syscall's
	// executable path. The Mesh hook checks its inode against the current PATH.
	var replacement [8]byte
	binaryle.LittleEndian.PutUint64(replacement[:], pathPtr)
	if _, err := syscall.PtracePokeData(pid, uintptr(argvPtr), replacement[:]); err != nil {
		if backend != "auto" {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		return false
	}
	if _, err := syscall.PtracePokeData(pid, uintptr(handoffAddress), []byte{'1'}); err != nil {
		binaryle.LittleEndian.PutUint64(replacement[:], argv0)
		if _, restoreErr := syscall.PtracePokeData(pid, uintptr(argvPtr), replacement[:]); restoreErr != nil || backend != "auto" {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		return false
	}
	regs.Rdi = proxyAddress
	if err := syscall.PtraceSetRegs(pid, &regs); err != nil {
		binaryle.LittleEndian.PutUint64(replacement[:], argv0)
		_, argvError := syscall.PtracePokeData(pid, uintptr(argvPtr), replacement[:])
		_, markerError := syscall.PtracePokeData(pid, uintptr(handoffAddress), []byte{'0'})
		if argvError != nil || markerError != nil || backend != "auto" {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		return false
	}
	return true
}
