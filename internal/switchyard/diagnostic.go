package switchyard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const maxDiagnosticSize = 1 << 20

type diagnosticRecord struct {
	Time      string `json:"time"`
	Phase     string `json:"phase"`
	Result    string `json:"result"`
	ElapsedMS int64  `json:"elapsed_ms"`
	ExitCode  *int   `json:"exit_code,omitempty"`
}

// Diagnostics contain only fixed phase/result labels and timings. In particular,
// never log commands, paths, cloud responses or the invocation environment.
func debugEvent(phase, result string, started time.Time) {
	if os.Getenv("SWITCHYARD_DEBUG") != "1" {
		return
	}
	home, err := dataHome()
	if err == nil {
		logEvent(home, phase, result, started)
	}
}

// Logging is best-effort, bounded and independent of command streams and exit
// status. A full disk, unsafe log path or contended logger never breaks a job.
func logEvent(home, phase, result string, started time.Time, exitCode ...int) {
	directory := filepath.Join(home, "logs")
	if privateTree(directory) != nil {
		return
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return
	}
	defer lock.Close()
	if !privateOwnedFile(lock) || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(directory, "events.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	if !privateOwnedFile(file) {
		return
	}
	elapsed := int64(0)
	if !started.IsZero() {
		elapsed = max(0, time.Since(started).Milliseconds())
	}
	record := diagnosticRecord{Time: time.Now().UTC().Format(time.RFC3339Nano), Phase: phase, Result: result, ElapsedMS: elapsed}
	if len(exitCode) != 0 {
		record.ExitCode = &exitCode[0]
	}
	data, err := json.Marshal(record)
	if err != nil {
		return
	}
	info, err := file.Stat()
	if err != nil {
		return
	}
	if info.Size()+int64(len(data))+1 > maxDiagnosticSize {
		previous := path + ".1"
		if old, err := os.Lstat(previous); err == nil {
			if !old.Mode().IsRegular() || old.Mode().Perm() != 0o600 || !ownedByCurrentUser(old) || old.Sys().(*syscall.Stat_t).Nlink != 1 {
				return
			}
		} else if !os.IsNotExist(err) {
			return
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(info, current) || os.Rename(path, previous) != nil {
			return
		}
		next, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
		if err == nil {
			defer next.Close()
			_, _ = next.Write(append(data, '\n'))
		}
		return
	}
	_, _ = file.Write(append(data, '\n'))
}

func privateOwnedFile(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode().IsRegular() && ownedByCurrentUser(info) && info.Mode().Perm() == 0o600 && info.Sys().(*syscall.Stat_t).Nlink == 1
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
