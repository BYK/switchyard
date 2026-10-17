package switchyard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

var hookTools = map[string]bool{
	"pnpm": true, "npm": true, "go": true, "cargo": true,
	"pytest": true, "uv": true, "make": true,
}

func realTool(tool, self string) (string, error) {
	if tool == "" || filepath.Base(tool) != tool || tool == "." || tool == ".." {
		return "", errors.New("invalid hooked command name")
	}
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if directory == "" {
			// An empty PATH component denotes the current working directory.
			directory = "."
		}
		path := filepath.Join(directory, tool)
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		actual, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if actual != self {
			return path, nil
		}
	}
	return "", errors.New("hooked tool was not found outside Switchyard shims")
}

func shimDirectory(home string) string {
	return filepath.Join(home, "shims")
}

func isTTY(file *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&termios)))
	return errno == 0
}

func installShims(home, self string) error {
	dir := shimDirectory(home)
	if err := privateTree(dir); err != nil {
		return err
	}
	names := []string{"switchyard-shell"}
	for name := range hookTools {
		names = append(names, name)
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if target, err := os.Readlink(path); err == nil && target == self {
			continue
		}
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("hook path %q already exists", path)
		} else if !os.IsNotExist(err) {
			return errors.New("could not inspect hook path")
		}
		if err := os.Symlink(self, path); err != nil {
			return errors.New("could not install command shim")
		}
	}
	return nil
}

func runHook(home, self string, args []string) int {
	if len(args) == 1 && args[0] == "install" {
		if err := installShims(home, self); err != nil {
			return fail(err)
		}
		fmt.Printf("Installed opt-in PATH shims and an experimental shell entrypoint in %s; no agent environment was changed.\n", shimDirectory(home))
		return 0
	}
	fromShell := len(args) > 0 && args[0] == "--shell"
	resolved := ""
	if fromShell {
		args = args[1:]
		if len(args) < 3 || args[0] != "--resolved" || !filepath.IsAbs(args[1]) {
			return fail(errors.New("Bash hook requires an absolute resolved command"))
		}
		resolved, args = args[1], args[2:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return fail(errors.New("hook needs a tool name"))
	}
	tool := args[0]
	command := args[1:]
	traced := os.Getenv("SWITCHYARD_TRACE_HANDOFF") == "1"
	if traced && !fromShell {
		if !filepath.IsAbs(tool) {
			return fail(errors.New("traced command needs an absolute resolved path"))
		}
		resolved, tool, fromShell = tool, filepath.Base(tool), true
	}
	real, lookupErr := realTool(tool, self)
	resolvedMismatch := false
	if fromShell {
		info, err := os.Stat(resolved)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fail(errors.New("Bash-resolved command is not executable"))
		}
		if lookupErr != nil {
			resolvedMismatch = true
		} else {
			pathInfo, err := os.Stat(real)
			resolvedMismatch = err != nil || !os.SameFile(info, pathInfo)
		}
		if resolvedMismatch {
			real = resolved
		}
	} else if lookupErr != nil {
		return fail(lookupErr)
	}
	backend := os.Getenv("SWITCHYARD_BACKEND")
	if backend == "" {
		backend = "auto"
	}
	if backend != "auto" && backend != "local" && !remoteName.MatchString(backend) {
		return fail(errors.New("invalid SWITCHYARD_BACKEND override"))
	}
	if resolvedMismatch && backend != "auto" && backend != "local" {
		return fail(errors.New("Bash resolved a different command than Switchyard; cannot offload"))
	}
	// A direct exec preserves PID, open descriptors, signals and exit status.
	// Unrecognized subcommands and ordinary PATH shims with a terminal stay local.
	interactive := isTTY(os.Stdin)
	previewed := os.Getenv("SWITCHYARD_TRACE_PREVIEWED") == "1"
	if traced && !previewed && backend != "auto" && backend != "local" {
		return fail(errors.New("preview and inspect every selected and excluded path before forced tracing"))
	}
	if backend == "auto" && traced && previewed && !resolvedMismatch {
		if cwd, err := os.Getwd(); err == nil {
			if selected := pressureReadyBackend(home, self, placementCommand{Directory: cwd, Argv: append([]string{tool}, command...)}); selected != "" {
				backend = selected
			}
		}
	}
	if backend == "local" || backend == "auto" || resolvedMismatch || traced && !previewed || interactive && !fromShell && !traced {
		if traced {
			if err := os.Setenv("SWITCHYARD_TRACE_BYPASS", "1"); err != nil {
				return fail(errors.New("could not mark local traced tool"))
			}
		}
		if err := syscall.Exec(real, append([]string{tool}, command...), os.Environ()); err != nil {
			return fail(errors.New("could not start local tool"))
		}
		return 0
	}
	start, err := os.Getwd()
	if err != nil {
		return fail(errors.New("could not locate command directory"))
	}
	if _, _, _, err := locateRepo(start); err != nil {
		if backend != "auto" {
			return fail(err)
		}
		if traced {
			if err := os.Setenv("SWITCHYARD_TRACE_BYPASS", "1"); err != nil {
				return fail(errors.New("could not mark local traced tool"))
			}
		}
		if err := syscall.Exec(real, append([]string{tool}, command...), os.Environ()); err != nil {
			return fail(errors.New("could not start local tool"))
		}
		return 0
	}
	logEvent(home, "placement", "remote", time.Time{})
	return executeJob(home, self, start, backend, nil, []string{"cpu"}, append([]string{tool}, command...), false, true)
}

type placementCommand struct {
	Directory string
	Argv      []string
}

func pressureReadyBackend(home, self string, command placementCommand) string {
	busy, _ := pressured()
	return pressureReadyBackendAtPressure(home, self, busy, command)
}

func pressureReadyBackendAtPressure(home, self string, busy bool, command ...placementCommand) string {
	if !busy {
		return ""
	}
	providers, err := providers(home, self)
	if err != nil {
		return ""
	}
	// Timings are diagnostics, never permission to try a remote worker. An
	// unseen command must offload under pressure without first earning samples.
	for _, provider := range providers {
		if isAWSWorker(provider.Name()) && !awsSupportsAutomaticCommand(command...) {
			continue
		}
		if provider.cfg.Kind == "local" || !supports(provider.cfg, []string{"cpu"}) || !provider.Ready(context.Background()) {
			continue
		}
		if isAWSWorker(provider.Name()) {
			available, err := awsWorkerAvailable(provider.Name())
			if err != nil || !available {
				if err == nil {
					awsRequestScale()
				}
				continue
			}
		}
		return provider.Name()
	}
	if awsSupportsAutomaticCommand(command...) {
		awsRequestWake()
	}
	return ""
}

func awsSupportsAutomaticCommand(command ...placementCommand) bool {
	if len(command) == 0 {
		return true
	}
	request := command[0]
	if len(request.Argv) == 0 {
		return false
	}
	// Managed AWS workers always use the pinned Node/pnpm image. A CPU slot
	// alone never proves that Go, Rust, or Python tools exist inside that image.
	manager := filepath.Base(request.Argv[0])
	switch manager {
	case "node", "npx":
		return true
	case "npm", "pnpm":
		if request.Directory == "" {
			return false
		}
		lock := "package-lock.json"
		if manager == "pnpm" {
			lock = "pnpm-lock.yaml"
		}
		// Reuse the image's setup checks so an incompatible project never wakes
		// that worker or turns a working native command into a setup failure.
		if _, err := containerPackageRoot(request.Directory, request.Directory, lock); err != nil {
			return false
		}
		return manager != "pnpm" || validatePinnedPnpm(request.Directory) == nil
	default:
		return false
	}
}
