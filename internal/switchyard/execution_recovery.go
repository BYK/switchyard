package switchyard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const automaticPreparationTimeout = 30 * time.Second

type jobAttempt struct {
	home, phase            string
	automatic, canRunLocal bool
	started, fallback      bool
}

func automaticInvocation(backend string, proxy bool) bool {
	return proxy && (backend == "auto" || os.Getenv("SWITCHYARD_TRACE_HANDOFF") == "1" && os.Getenv("SWITCHYARD_BACKEND") == "auto")
}

func (attempt *jobAttempt) fail(err error) int {
	if !attempt.automatic {
		return fail(err)
	}
	var transfer *snapshotTransferError
	if errors.As(err, &transfer) {
		logEvent(attempt.home, attempt.phase, transfer.reason, time.Time{}, transfer.exitCode)
	} else {
		logEvent(attempt.home, attempt.phase, "failed", time.Time{})
	}
	// Execute is the point of no return. A lost response never authorizes a
	// second execution: reconcile the saved status instead of duplicating work.
	attempt.fallback = !attempt.started && attempt.canRunLocal
	return 2
}

func (attempt *jobAttempt) retry(ctx context.Context, phase string, operation func(context.Context) error) error {
	limit := 1
	if attempt.automatic {
		limit = 2
	}
	var err error
	for index := 0; index < limit; index++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err = operation(ctx)
		if err == nil {
			return nil
		}
		if index+1 < limit {
			logEvent(attempt.home, phase, "retry", time.Time{})
		}
	}
	return err
}

func executeJob(home, self, start, backend string, includes, required []string, argv []string, details, proxy bool, outputs ...string) int {
	attempt := &jobAttempt{home: home, phase: "placement", automatic: automaticInvocation(backend, proxy), canRunLocal: len(argv) != 0}
	for _, capability := range required {
		if capability != "cpu" {
			attempt.canRunLocal = false
		}
	}
	exit := executeJobAttempt(home, self, start, backend, includes, required, argv, details, proxy, attempt, outputs...)
	if !attempt.fallback {
		return exit
	}
	// Return through the attempt's defers before exec, releasing its captured
	// snapshot, leases and signal handlers even when preparation failed.
	logEvent(home, "fallback", "local", time.Time{})
	if os.Chdir(start) != nil {
		logEvent(home, "local-command", "directory-unavailable", time.Time{})
		return 126
	}
	path, err := exec.LookPath(argv[0])
	if filepath.Base(argv[0]) == argv[0] {
		path, err = realTool(argv[0], self)
	}
	if err != nil {
		logEvent(home, "local-command", "missing", time.Time{})
		return 127
	}
	if syscall.Exec(path, argv, os.Environ()) != nil {
		logEvent(home, "local-command", "failed", time.Time{})
		return 126
	}
	return 0
}
