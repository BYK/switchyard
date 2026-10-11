package switchyard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkerLoadProtectsPreparedAndRunningJobs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "worker")
	if err := initWorker(root); err != nil {
		t.Fatal(err)
	}
	project, job := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	if err := workerPrepare(root, project, job); err != nil {
		t.Fatal(err)
	}
	active, err := workerLoad(root)
	if err != nil || active != 1 {
		t.Fatalf("prepared job must protect worker from stopping: %d, %v", active, err)
	}
	path, err := jobPath(root, project, job)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJob(path, jobRecord{ID: job, Project: project, State: "finished"}); err != nil {
		t.Fatal(err)
	}
	active, err = workerLoad(root)
	if err != nil || active != 1 {
		t.Fatalf("recent result must remain accessible: %d, %v", active, err)
	}
	old := time.Now().Add(-16 * time.Minute)
	if err := os.Chtimes(filepath.Join(path, "job.json"), old, old); err != nil {
		t.Fatal(err)
	}
	active, err = workerLoad(root)
	if err != nil || active != 0 {
		t.Fatalf("old result must release capacity: %d, %v", active, err)
	}
}
