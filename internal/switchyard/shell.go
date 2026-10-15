package switchyard

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var shellWord = regexp.MustCompile(`^[a-zA-Z0-9_./:@+=-]+$`)

// Only single, literal tool calls can move. A shell expression that reads
// files, sets variables, redirects streams or chains commands stays local.
func shellCommand(script string) []string {
	if strings.ContainsAny(script, "\n\r\t") {
		return nil
	}
	words := strings.Fields(script)
	if len(words) == 0 {
		return nil
	}
	for _, word := range words {
		if !shellWord.MatchString(word) {
			return nil
		}
	}
	tool := filepath.Base(words[0])
	if !hookTools[tool] {
		return nil
	}
	if strings.Contains(words[0], "/") && !filepath.IsAbs(words[0]) {
		return nil
	}
	return words
}

func runShell(home, self string, args []string) int {
	if os.Getenv("SWITCHYARD_CONTAINER_IMAGE") != "" {
		return runContainerShell(home, args)
	}
	local := func() int {
		if err := syscall.Exec("/bin/bash", append([]string{"bash"}, args...), os.Environ()); err != nil {
			return fail(errors.New("could not start local shell"))
		}
		return 0
	}
	if len(args) != 2 || args[0] != "-c" || isTTY(os.Stdin) {
		return local()
	}
	argv := shellCommand(args[1])
	if len(argv) == 0 {
		return local()
	}
	backend := os.Getenv("SWITCHYARD_BACKEND")
	if backend == "" {
		backend = "auto"
	}
	if backend == "local" {
		return local()
	}
	if backend != "auto" && !remoteName.MatchString(backend) {
		return fail(errors.New("invalid SWITCHYARD_BACKEND override"))
	}
	start, err := os.Getwd()
	if err != nil {
		return fail(errors.New("could not locate command directory"))
	}
	root, _, _, err := locateRepo(start)
	if err != nil {
		if backend == "auto" {
			return local()
		}
		return fail(err)
	}
	// Jobs currently execute from the captured worktree root. Keep subdirectory
	// calls local rather than silently running a different package's command.
	if backend == "auto" && root != start {
		return local()
	}
	// A custom absolute executable must never become a different worker tool
	// merely because it has the same basename as a supported command.
	if backend == "auto" && filepath.IsAbs(argv[0]) {
		return local()
	}
	// Startup files and exported functions can replace a literal tool name.
	// Keep native Bash in charge whenever its resolution is not predictable.
	if os.Getenv("BASH_ENV") != "" || os.Getenv("BASH_FUNC_"+filepath.Base(argv[0])+"%%") != "" {
		return local()
	}
	// An absolute tool path can differ between local and remote images; use
	// the worker's tool of the same name and validate its version per profile.
	if backend != "auto" {
		argv[0] = filepath.Base(argv[0])
	}
	// Automatic dispatch selects under pressure and validates a fresh snapshot
	// before transfer; no caller-supplied preview assertion or timings are needed.
	return executeJob(home, self, start, backend, nil, []string{"cpu"}, argv, false, true)
}
