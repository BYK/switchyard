package switchyard

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// A saved patch and log are the durable result. Checkouts, dependency trees and
// incoming files are disposable only after that result exists. In particular,
// failed edit collection must retain the checkout for recovery.
func releaseJobTrees(directory string, record jobRecord) error {
	if (record.State != "finished" && record.State != "setup-failed") || record.EditsError != "" || record.ExitStatus == nil && record.SetupStatus == nil {
		return nil
	}
	patch, err := os.Lstat(filepath.Join(directory, "changes.patch"))
	if err != nil || !patch.Mode().IsRegular() || !ownedByCurrentUser(patch) || patch.Mode().Perm() != 0o600 || patch.Size() > maxPatchSize || (patch.Size() != 0) != record.HasEdits {
		return errors.New("job patch is not saved")
	}
	log, err := os.Lstat(filepath.Join(directory, "output.log"))
	if err != nil || !log.Mode().IsRegular() || !ownedByCurrentUser(log) || log.Mode().Perm() != 0o600 {
		return errors.New("job log is not saved")
	}
	// Rename alone is not crash durability. Flush every result and its directory
	// before deleting the only writable copy of the command's edits.
	for _, name := range []string{"job.json", "changes.patch", "output.log"} {
		file, err := os.OpenFile(filepath.Join(directory, name), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return errors.New("job result is not saved")
		}
		valid := privateOwnedFile(file)
		if valid {
			err = file.Sync()
		}
		closeErr := file.Close()
		if !valid || err != nil || closeErr != nil {
			return errors.New("job result is not durable")
		}
	}
	dir, err := os.OpenFile(directory, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return errors.New("job result directory is unavailable")
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("job result directory is not durable")
	}
	return removeJobTrees(directory)
}

func removeJobTrees(directory string) error {
	for _, name := range []string{"incoming", "worktree"} {
		path := filepath.Join(directory, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(info) {
			return errors.New("job data directory is unsafe")
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("could not reclaim completed job data")
		}
	}
	return nil
}

// Execute and discard share this nonblocking lease. Absence of job.json alone
// never proves safety: an execution could be creating its first status record.
func jobExecutionLease(directory string) (*os.File, error) {
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || !ownedByCurrentUser(directoryInfo) || directoryInfo.Mode().Perm() != 0o700 {
		return nil, errors.New("job directory is not private")
	}
	file, err := os.OpenFile(filepath.Join(directory, ".execution.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, errors.New("could not lock job execution")
	}
	if !privateOwnedFile(file) || syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		file.Close()
		return nil, errors.New("job execution is locked or unsafe")
	}
	return file, nil
}

func discardPreparedJob(root, project, job string) error {
	if err := workerReady(root); err != nil {
		return err
	}
	directory, err := jobPath(root, project, job)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(directory); os.IsNotExist(err) {
		return nil
	}
	lease, err := jobExecutionLease(directory)
	if err != nil {
		return err
	}
	defer lease.Close()
	if _, err := os.Lstat(filepath.Join(directory, "job.json")); !os.IsNotExist(err) {
		return errors.New("job already has execution history")
	}
	if err := removeJobTrees(directory); err != nil {
		return err
	}
	// Retain the lease inode and a tombstone. Reusing this job ID can never
	// authorize a delayed execution or make the idle scanner count it as busy.
	return writeJob(directory, jobRecord{ID: job, Project: project, State: "discarded", Finished: time.Now().UTC().Format(time.RFC3339Nano)})
}
