package mesh

import (
	"context"
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
	if len(words) < 2 {
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
	if os.Getenv("MESH_CONTAINER_IMAGE") != "" {
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
	backend := os.Getenv("MESH_BACKEND")
	if backend == "" {
		backend = "auto"
	}
	if backend == "local" || backend == "auto" && os.Getenv("MESH_TRACE_PREVIEWED") != "1" {
		return local()
	}
	if backend != "auto" && !remoteName.MatchString(backend) {
		return fail(errors.New("invalid MESH_BACKEND override"))
	}
	start, err := os.Getwd()
	if err != nil {
		return fail(errors.New("could not locate command directory"))
	}
	if _, _, _, err := locateRepo(start); err != nil {
		if backend == "auto" {
			return local()
		}
		return fail(err)
	}
	if backend == "auto" {
		real, lookupErr := realTool(filepath.Base(argv[0]), self)
		if lookupErr != nil {
			return local()
		}
		key, ok := commandSignature(start, real, argv[1:])
		if !ok {
			return local()
		}
		backend = observedReadyBackend(home, self, key)
		if backend == "" {
			return local()
		}
	}
	provider, _, err := selectProvider(context.Background(), home, self, backend, []string{"cpu"})
	if err != nil {
		return fail(err)
	}
	if provider.Name() == "local" {
		return local()
	}
	// An absolute tool path can differ between local and remote images; use
	// the worker's tool of the same name and validate its version per profile.
	argv[0] = filepath.Base(argv[0])
	return executeJob(home, self, start, provider.Name(), nil, []string{"cpu"}, argv, false, true)
}
