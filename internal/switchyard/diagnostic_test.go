package switchyard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDiagnosticsRotateAndRejectUnsafeFiles(t *testing.T) {
	home := t.TempDir()
	logEvent(home, "transfer", "retry", time.Time{})
	path := filepath.Join(home, "logs", "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil || !json.Valid([]byte(strings.TrimSpace(string(data)))) {
		t.Fatalf("diagnostic was not written: %v", err)
	}
	recoveryWrite(t, path, strings.Repeat("x", maxDiagnosticSize), 0o600)
	logEvent(home, "fallback", "local", time.Time{})
	old, err := os.Stat(path + ".1")
	if err != nil || old.Size() != maxDiagnosticSize {
		t.Fatalf("diagnostic rotation failed: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 || info.Size() >= maxDiagnosticSize {
		t.Fatalf("new diagnostic is not private and bounded: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	recoveryWrite(t, outside, "unchanged", 0o600)
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	logEvent(home, "transfer", "failed", time.Time{})
	if data, _ := os.ReadFile(outside); string(data) != "unchanged" {
		t.Fatal("logger followed a symlink")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, path); err != nil {
		t.Fatal(err)
	}
	logEvent(home, "transfer", "failed", time.Time{})
	if data, _ := os.ReadFile(outside); string(data) != "unchanged" {
		t.Fatal("logger wrote through a hard link")
	}
}

func TestDiagnosticsNeverBlockOnContentionOrFIFO(t *testing.T) {
	home := t.TempDir()
	logEvent(home, "placement", "local", time.Time{})
	lock, err := os.OpenFile(filepath.Join(home, "logs", ".lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	logEvent(home, "transfer", "retry", time.Time{})
	if time.Since(start) > time.Second {
		t.Fatal("logger waited for its lock")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "logs", "events.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	logEvent(home, "transfer", "failed", time.Time{})
}
