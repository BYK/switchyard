package switchyard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReleaseJobTreesRequiresDurableResults(t *testing.T) {
	for _, scenario := range []struct {
		name, state, editsError         string
		missing, missingStatus, symlink bool
		retained                        bool
	}{
		{"finished", "finished", "", false, false, false, false},
		{"failed setup", "setup-failed", "", false, false, false, false},
		{"running", "running", "", false, false, false, true},
		{"uncaptured edits", "finished", "patch failed", false, false, false, true},
		{"missing patch", "finished", "", true, false, false, true},
		{"missing status", "finished", "", false, true, false, true},
		{"symlink patch", "finished", "", false, false, true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			directory := t.TempDir()
			for _, tree := range []string{"incoming", "worktree"} {
				if err := os.Mkdir(filepath.Join(directory, tree), 0o700); err != nil {
					t.Fatal(err)
				}
				recoveryWrite(t, filepath.Join(directory, tree, "valuable"), "preserved until captured", 0o600)
			}
			recoveryWrite(t, filepath.Join(directory, "output.log"), "job output", 0o600)
			if !scenario.missingStatus {
				recoveryWrite(t, filepath.Join(directory, "job.json"), "{}", 0o600)
			}
			if !scenario.missing {
				if scenario.symlink {
					outside := filepath.Join(t.TempDir(), "outside")
					recoveryWrite(t, outside, "patch", 0o600)
					if err := os.Symlink(outside, filepath.Join(directory, "changes.patch")); err != nil {
						t.Fatal(err)
					}
				} else {
					recoveryWrite(t, filepath.Join(directory, "changes.patch"), "patch", 0o600)
				}
			}
			code := 37
			record := jobRecord{State: scenario.state, EditsError: scenario.editsError, HasEdits: true, ExitStatus: &code}
			if scenario.state == "setup-failed" {
				record.ExitStatus, record.SetupStatus = nil, &code
			}
			err := releaseJobTrees(directory, record)
			if !scenario.retained && err != nil {
				t.Fatal(err)
			}
			for _, tree := range []string{"incoming", "worktree"} {
				_, err := os.Stat(filepath.Join(directory, tree))
				if scenario.retained && err != nil || !scenario.retained && !os.IsNotExist(err) {
					t.Fatalf("unexpected retention for %s: %v", tree, err)
				}
			}
			if _, err := os.Stat(filepath.Join(directory, "output.log")); err != nil {
				t.Fatal("cleanup lost the saved log")
			}
		})
	}
}

func TestDiscardPreparationCannotRaceExecution(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worker")
	project, job := strings.Repeat("a", 32), strings.Repeat("b", 32)
	if err := initWorker(root); err != nil {
		t.Fatal(err)
	}
	if err := workerPrepare(root, project, job); err != nil {
		t.Fatal(err)
	}
	directory, err := jobPath(root, project, job)
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(directory, "incoming", "files", "payload")
	recoveryWrite(t, payload, "must survive active execution", 0o600)
	lease, err := jobExecutionLease(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := discardPreparedJob(root, project, job); err == nil {
		t.Fatal("discard ignored an active execution lease")
	}
	if _, err := os.Stat(payload); err != nil {
		t.Fatal("discard touched active job data")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := discardPreparedJob(root, project, job); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(payload); !os.IsNotExist(err) {
		t.Fatal("discard retained a partial snapshot")
	}
	if load, err := workerLoad(root); err != nil || load != 0 {
		t.Fatalf("discarded preparation prevents idle stop: load=%d err=%v", load, err)
	}
	if code, err := workerExecute(root, project, job, strings.Repeat("c", 64), ".", []string{"false"}, nil, nil, true); err == nil || code != 2 {
		t.Fatal("discarded job accepted a delayed execution")
	}
}
