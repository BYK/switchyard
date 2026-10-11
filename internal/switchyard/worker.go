package switchyard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const workerMarker = "switchyard-worker-v1\n"

type jobRecord struct {
	ID              string `json:"id"`
	Project         string `json:"project"`
	Snapshot        string `json:"snapshot"`
	State           string `json:"state"`
	ExitStatus      *int   `json:"exit_status"`
	SetupStatus     *int   `json:"setup_status,omitempty"`
	Started         string `json:"started"`
	Finished        string `json:"finished,omitempty"`
	PID             int    `json:"pid,omitempty"`
	ProcessIdentity string `json:"process_identity,omitempty"`
	HasEdits        bool   `json:"has_edits"`
	EditsError      string `json:"edits_error,omitempty"`
	LogTruncated    bool   `json:"log_truncated,omitempty"`
}

func rootPath(root string) error {
	if !filepath.IsAbs(root) || strings.Contains(root, "..") {
		return errors.New("worker root must be an absolute private directory")
	}
	return nil
}

func workerReady(root string) error {
	if err := rootPath(root); err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return errors.New("worker root is not initialized")
	}
	marker, err := privateRead(filepath.Join(root, ".switchyard-worker"), 100)
	if err != nil || string(marker) != workerMarker {
		return errors.New("worker root has no ownership marker")
	}
	return nil
}

func initWorker(root string) error {
	if err := rootPath(root); err != nil {
		return err
	}
	if info, err := os.Lstat(root); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("worker root must be a private directory")
		}
		if _, markerErr := os.Lstat(filepath.Join(root, ".switchyard-worker")); os.IsNotExist(markerErr) {
			entries, err := os.ReadDir(root)
			if err != nil || len(entries) > 0 {
				return errors.New("worker root already contains unrelated files")
			}
		} else if markerErr != nil || workerReady(root) != nil {
			return errors.New("worker root has an invalid ownership marker")
		}
	} else if !os.IsNotExist(err) {
		return errors.New("could not inspect worker root")
	}
	if err := privateTree(root); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(root, ".switchyard-worker"), []byte(workerMarker)); err != nil {
		return err
	}
	return privateTree(filepath.Join(root, "projects"))
}

func projectPath(root, project string) (string, error) {
	if err := requireID(project); err != nil {
		return "", err
	}
	return filepath.Join(root, "projects", project), nil
}

func jobPath(root, project, job string) (string, error) {
	projectRoot, err := projectPath(root, project)
	if err != nil {
		return "", err
	}
	if err := requireID(job); err != nil {
		return "", err
	}
	return filepath.Join(projectRoot, "jobs", job), nil
}

func workerPrepare(root, project, job string) error {
	if err := workerReady(root); err != nil {
		return err
	}
	base, err := projectPath(root, project)
	if err != nil {
		return err
	}
	if err := requireID(job); err != nil {
		return err
	}
	if err := privateTree(filepath.Join(base, "jobs")); err != nil {
		return err
	}
	path := filepath.Join(base, "jobs", job)
	if err := os.Mkdir(path, 0o700); err != nil {
		return errors.New("job ID already exists or cannot be created")
	}
	dest := filepath.Join(path, "incoming", "files")
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	return withProjectLock(base, func() error {
		latest, err := privateRead(filepath.Join(base, "latest"), 100)
		if err == nil {
			snapshot := strings.TrimSpace(string(latest))
			if err := requireSnapshot(snapshot); err != nil {
				return err
			}
			if err := copyTree(filepath.Join(base, "mirrors", snapshot), dest); err != nil {
				return errors.New("could not seed the worker from its last snapshot")
			}
		} else if _, statErr := os.Lstat(filepath.Join(base, "latest")); !os.IsNotExist(statErr) {
			return errors.New("worker snapshot index is invalid")
		}
		return pruneOldJobs(base)
	})
}

func withProjectLock(base string, action func() error) error {
	if err := privateTree(base); err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(base, ".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return errors.New("could not lock worker project")
	}
	file := os.NewFile(uintptr(fd), ".lock")
	defer file.Close()
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return errors.New("could not lock worker project")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return action()
}

func pruneOldJobs(base string) error {
	entries, err := os.ReadDir(filepath.Join(base, "jobs"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || requireID(entry.Name()) != nil {
			continue
		}
		path := filepath.Join(base, "jobs", entry.Name())
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < 7*24*time.Hour {
			continue
		}
		content, err := privateRead(filepath.Join(path, "job.json"), 1<<20)
		if err != nil {
			if _, statErr := os.Lstat(filepath.Join(path, "job.json")); os.IsNotExist(statErr) {
				if err := os.RemoveAll(path); err != nil {
					return errors.New("could not reclaim abandoned worker transfer")
				}
			}
			continue
		}
		var record jobRecord
		if json.Unmarshal(content, &record) != nil || record.ID != entry.Name() || record.State != "finished" && record.State != "running" && record.State != "starting" {
			continue
		}
		if record.State == "running" && record.PID > 0 && record.ProcessIdentity != "" && record.ProcessIdentity == processIdentity(record.PID) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("could not reclaim old worker job")
		}
	}
	return nil
}

func verifyTree(tree string) (manifest, error) {
	content, err := privateRead(filepath.Join(tree, manifestName), 16<<20)
	if err != nil {
		return manifest{}, errors.New("snapshot manifest is missing")
	}
	var m manifest
	if err := json.Unmarshal(content, &m); err != nil || m.Version != 1 || requireSnapshot(m.Snapshot) != nil {
		return manifest{}, errors.New("invalid snapshot manifest")
	}
	computed, err := snapshotHash(m)
	if err != nil || computed != m.Snapshot {
		return manifest{}, errors.New("snapshot identity does not match its manifest")
	}
	seen := map[string]bool{}
	var total int64
	for _, entry := range m.Files {
		if safeRel(entry.Path) != nil || excludedPath(entry.Path) || seen[entry.Path] || entry.Size < 0 || entry.Size > maxFileSize || entry.Mode != 0o644 && entry.Mode != 0o755 {
			return manifest{}, errors.New("snapshot manifest includes an invalid file")
		}
		seen[entry.Path] = true
		if err := checkNoSymlinks(tree, entry.Path); err != nil {
			return manifest{}, err
		}
		hash, info, err := digestFile(tree, entry.Path)
		if err != nil || hash != entry.Hash || info.Size() != entry.Size || info.Mode().Perm() != os.FileMode(entry.Mode) {
			return manifest{}, errors.New("snapshot file differs from its manifest")
		}
		total += entry.Size
		if total > maxSnapshotSize {
			return manifest{}, errors.New("snapshot exceeds its size limit")
		}
	}
	actual := 0
	err = filepath.WalkDir(tree, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if item.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(tree, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != manifestName {
			if !seen[rel] || item.Type()&os.ModeSymlink != 0 {
				return errors.New("snapshot contains an unexpected file")
			}
			actual++
		}
		return nil
	})
	if err != nil || actual != len(m.Files) {
		return manifest{}, errors.New("snapshot contains unexpected or missing files")
	}
	return m, nil
}

func jobMetadata(root, project, job string) (jobRecord, error) {
	path, err := jobPath(root, project, job)
	if err != nil {
		return jobRecord{}, err
	}
	content, err := privateRead(filepath.Join(path, "job.json"), 1<<20)
	if err != nil {
		return jobRecord{}, errors.New("unknown job")
	}
	var record jobRecord
	if err := json.Unmarshal(content, &record); err != nil || record.ID != job || record.Project != project {
		return jobRecord{}, errors.New("invalid worker job record")
	}
	return record, nil
}

func writeJob(path string, job jobRecord) error {
	content, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(path, "job.json"), append(content, '\n'))
}

func processIdentity(pid int) string {
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	index := strings.LastIndex(string(content), ") ")
	if index < 0 {
		return ""
	}
	fields := strings.Fields(string(content[index+2:]))
	if len(fields) <= 19 {
		return ""
	}
	return fields[19]
}

func processSession(pid int) int {
	content, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	index := strings.LastIndex(string(content), ") ")
	if index < 0 {
		return -1
	}
	fields := strings.Fields(string(content[index+2:]))
	if len(fields) <= 3 {
		return -1
	}
	session, err := strconv.Atoi(fields[3])
	if err != nil {
		return -1
	}
	return session
}

func workerStatus(root, project, job string) (jobRecord, error) {
	if err := workerReady(root); err != nil {
		return jobRecord{}, err
	}
	record, err := jobMetadata(root, project, job)
	if err != nil {
		return jobRecord{}, err
	}
	if record.State == "running" && (record.ProcessIdentity == "" || record.ProcessIdentity != processIdentity(record.PID)) {
		record.State = "lost"
		record.ExitStatus = nil
	}
	return record, nil
}

// A prepared job has no job.json until execution starts. Treat it as occupied,
// and fail closed on invalid metadata: an idle stop must never interrupt work.
func workerLoad(root string) (int, error) {
	if err := workerReady(root); err != nil {
		return 0, err
	}
	projects, err := os.ReadDir(filepath.Join(root, "projects"))
	if err != nil || len(projects) > 4096 {
		return 0, errors.New("could not inspect worker projects")
	}
	active, inspected := 0, 0
	for _, project := range projects {
		if !project.IsDir() || requireID(project.Name()) != nil {
			return 0, errors.New("invalid worker project")
		}
		jobs, err := os.ReadDir(filepath.Join(root, "projects", project.Name(), "jobs"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return 0, errors.New("could not inspect worker jobs")
		}
		inspected += len(jobs)
		if inspected > 4096 {
			return 0, errors.New("too many worker jobs to inspect")
		}
		for _, job := range jobs {
			if !job.IsDir() || requireID(job.Name()) != nil {
				return 0, errors.New("invalid worker job")
			}
			path := filepath.Join(root, "projects", project.Name(), "jobs", job.Name(), "job.json")
			if _, err := os.Lstat(path); os.IsNotExist(err) {
				active++
				continue
			} else if err != nil {
				return 0, errors.New("could not inspect worker job")
			}
			record, err := workerStatus(root, project.Name(), job.Name())
			if err != nil {
				return 0, err
			}
			switch record.State {
			case "finished", "setup-failed", "lost":
				// The controller still retrieves logs and the patch after a
				// process exits. Keep a recent result available through that
				// transfer, including when the controller process crashes.
				info, err := os.Lstat(path)
				if err != nil || !info.Mode().IsRegular() {
					return 0, errors.New("invalid worker job metadata")
				}
				if time.Since(info.ModTime()) < awsIdleWindow {
					active++
				}
			case "starting", "running":
				active++
			default:
				return 0, errors.New("unknown worker job state")
			}
		}
	}
	return active, nil
}

func workerCancel(root, project, job string) (jobRecord, error) {
	record, err := workerStatus(root, project, job)
	if err != nil || record.State != "running" {
		return record, err
	}
	if identity := processIdentity(record.PID); identity == "" || identity != record.ProcessIdentity {
		return workerStatus(root, project, job)
	}
	if processSession(record.PID) != record.PID {
		return record, errors.New("worker process identity changed; refusing to signal")
	}
	if err := syscall.Kill(-record.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return record, errors.New("could not cancel worker job")
	}
	if path, err := jobPath(root, project, job); err == nil {
		stopJobContainer(path, project, job)
	}
	return record, nil
}

func workerFile(root, project, job, name string) ([]byte, error) {
	if err := workerReady(root); err != nil {
		return nil, err
	}
	path, err := jobPath(root, project, job)
	if err != nil {
		return nil, err
	}
	if _, err := jobMetadata(root, project, job); err != nil {
		return nil, err
	}
	limit := int64(maxLogSize)
	if name == "changes.patch" {
		limit = maxPatchSize
	}
	return privateRead(filepath.Join(path, name), limit)
}

type jobLog struct {
	mu        sync.Mutex
	file      *os.File
	count     int64
	truncated bool
}

type logWriter struct {
	log    *jobLog
	stream io.Writer
}

func (writer logWriter) Write(data []byte) (int, error) {
	writer.log.mu.Lock()
	defer writer.log.mu.Unlock()
	if writer.log.count < maxLogSize {
		kept := int64(len(data))
		if kept > maxLogSize-writer.log.count {
			kept = maxLogSize - writer.log.count
		}
		if _, err := writer.log.file.Write(data[:kept]); err != nil {
			return 0, err
		}
		writer.log.count += kept
	}
	if writer.log.count == maxLogSize {
		writer.log.truncated = true
	}
	return writer.stream.Write(data)
}

func resolvedWorkerCommand(name, target string, environment []string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		return name, nil
	}
	search := "/usr/bin:/bin"
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			search = value
			break
		}
	}
	for _, directory := range strings.Split(search, string(os.PathListSeparator)) {
		if directory == "" {
			directory = "."
		}
		if !filepath.IsAbs(directory) {
			directory = filepath.Join(target, directory)
		}
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("worker executable not found in the invocation PATH")
}

func nodeCommand(checkout, target, marker string, argv, environment []string) (*exec.Cmd, bool, error) {
	manager := filepath.Base(argv[0])
	if manager != "pnpm" && manager != "npm" {
		executable, err := resolvedWorkerCommand(argv[0], target, environment)
		if err != nil {
			return nil, false, err
		}
		return exec.Command(executable, argv[1:]...), false, nil
	}
	lock := "pnpm-lock.yaml"
	if manager == "npm" {
		lock = "package-lock.json"
	}
	root := ""
	for directory := target; ; directory = filepath.Dir(directory) {
		packageInfo, packageErr := os.Lstat(filepath.Join(directory, "package.json"))
		lockInfo, lockErr := os.Lstat(filepath.Join(directory, lock))
		if packageErr == nil && lockErr == nil && packageInfo.Mode().IsRegular() && lockInfo.Mode().IsRegular() {
			root = directory
			break
		}
		if directory == checkout {
			break
		}
	}
	if root == "" {
		return nil, true, errors.New("Node dependency setup needs a package.json and matching lockfile in the snapshot")
	}
	// The marker is outside the checkout. The real command starts only after
	// dependency setup succeeds in this same, cancellable process group.
	script := `cd "$1" || exit 2
if [ "$2" = pnpm ]; then
  "$2" install --frozen-lockfile --prefer-offline || exit $?
else
  "$2" ci --prefer-offline --no-audit --no-fund || exit $?
fi
cd "$4" || exit 2
: > "$3" || exit 2
shift 4
exec "$@"`
	params := append([]string{"-c", script, "switchyard-node-setup", root, manager, marker, target}, argv...)
	return exec.Command("/bin/sh", params...), true, nil
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		return exited.ExitCode()
	}
	return 127
}

func workerExecute(root, project, job, snapshot, cwd string, argv, environment []string, stdin io.Reader) (int, error) {
	if err := workerReady(root); err != nil {
		return 2, err
	}
	if environment == nil || validateEnvironment(environment) != nil {
		return 2, errors.New("worker command environment is missing or invalid")
	}
	if err := requireSnapshot(snapshot); err != nil {
		return 2, err
	}
	if len(argv) == 0 || cwd != "." && safeRel(cwd) != nil {
		return 2, errors.New("invalid command or working directory")
	}
	path, err := jobPath(root, project, job)
	if err != nil {
		return 2, err
	}
	incoming := filepath.Join(path, "incoming", "files")
	m, err := verifyTree(incoming)
	if err != nil || m.Snapshot != snapshot {
		return 2, errors.New("worker received a different or invalid snapshot")
	}
	if err := os.Remove(filepath.Join(incoming, manifestName)); err != nil {
		return 2, errors.New("could not finalize snapshot input")
	}
	checkout := filepath.Join(path, "worktree")
	if err := copyTree(incoming, checkout); err != nil {
		return 2, errors.New("could not create isolated job checkout")
	}
	target := filepath.Join(checkout, filepath.FromSlash(cwd))
	info, err := os.Lstat(target)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 2, errors.New("command directory does not exist in the snapshot")
	}
	record := jobRecord{ID: job, Project: project, Snapshot: snapshot, State: "starting", Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeJob(path, record); err != nil {
		return 2, err
	}
	log, err := os.OpenFile(filepath.Join(path, "output.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 2, errors.New("could not create job log")
	}
	output := &jobLog{file: log}
	var image workerImage
	if _, err := os.Lstat(imageFile(root)); err == nil {
		image, err = loadImage(root)
		if err != nil {
			log.Close()
			return 2, err
		}
	} else if !os.IsNotExist(err) {
		log.Close()
		return 2, errors.New("worker image configuration is unavailable")
	}
	var cmd *exec.Cmd
	var setup bool
	var planErr error
	if image.ID != "" {
		cmd, setup, planErr = containerJobCommand(root, path, checkout, cwd, argv, environment, image)
	} else {
		cmd, setup, planErr = nodeCommand(checkout, target, filepath.Join(path, "setup-ready"), argv, environment)
	}
	if planErr != nil {
		fmt.Fprintln(logWriter{log: output, stream: os.Stderr}, "switchyard: "+planErr.Error())
		record.State, record.SetupStatus, record.Finished = "setup-failed", new(int), time.Now().UTC().Format(time.RFC3339Nano)
		*record.SetupStatus = 2
		if err := log.Close(); err != nil {
			return 2, errors.New("could not close job log")
		}
		return 2, writeJob(path, record)
	}
	defer func() {
		for _, file := range cmd.ExtraFiles {
			file.Close()
		}
	}()
	if image.ID == "" {
		cmd.Dir = target
	}
	var forwardedInput *os.File
	if source, ok := stdin.(*os.File); ok {
		cmd.Stdin = source
	} else {
		input, output, pipeErr := os.Pipe()
		if pipeErr != nil {
			log.Close()
			return 2, errors.New("could not prepare command input")
		}
		cmd.Stdin = input
		forwardedInput = output
		defer input.Close()
		defer output.Close()
	}
	cmd.Stdout = logWriter{log: output, stream: os.Stdout}
	cmd.Stderr = logWriter{log: output, stream: os.Stderr}
	if image.ID == "" {
		cmd.Env = workerEnvironment(environment, target)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	status := 127
	if err := cmd.Start(); err == nil {
		if forwardedInput != nil {
			cmd.Stdin.(*os.File).Close()
			go func() {
				defer forwardedInput.Close()
				_, _ = io.Copy(forwardedInput, stdin)
			}()
		}
		identity := processIdentity(cmd.Process.Pid)
		if identity == "" {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			cmd.Wait()
			log.Close()
			return 2, errors.New("could not identify the worker process group")
		}
		record.State, record.PID, record.ProcessIdentity = "running", cmd.Process.Pid, identity
		if err := writeJob(path, record); err != nil {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			cmd.Wait()
			log.Close()
			return 2, err
		}
		stops := make(chan os.Signal, 1)
		signal.Notify(stops, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			select {
			case <-stops:
				syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			case <-done:
			}
		}()
		status = exitStatus(cmd.Wait())
		if image.ID != "" {
			stopJobContainer(path, project, job)
		}
		if forwardedInput != nil {
			forwardedInput.Close()
		}
		close(done)
		signal.Stop(stops)
	}
	if err := log.Close(); err != nil {
		return 2, errors.New("could not close job log")
	}
	record.LogTruncated = output.truncated
	record.EditsError = collectPatch(incoming, checkout, filepath.Join(path, "changes.patch"))
	if record.EditsError == "" {
		if info, err := os.Stat(filepath.Join(path, "changes.patch")); err == nil {
			record.HasEdits = info.Size() > 0
		}
	}
	// A failed mirror update never changes the result of a completed command.
	if err := publishMirror(root, project, snapshot, incoming); err != nil {
		fmt.Fprintln(os.Stderr, "switchyard: worker cache could not be updated")
	}
	if setup {
		marker := filepath.Join(path, "setup-ready")
		if image.ID != "" {
			marker = filepath.Join(path, "setup", "ready")
		}
		if _, err := os.Lstat(marker); err != nil {
			record.State, record.SetupStatus = "setup-failed", &status
		}
	}
	if record.State != "setup-failed" {
		record.State, record.ExitStatus = "finished", &status
	}
	record.Finished = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeJob(path, record); err != nil {
		return 2, err
	}
	if record.State == "setup-failed" {
		if status >= 128 {
			return status, nil
		}
		return 2, nil
	}
	return status, nil
}

func publishMirror(root, project, snapshot, input string) error {
	base, err := projectPath(root, project)
	if err != nil {
		return err
	}
	return withProjectLock(base, func() error {
		mirrors := filepath.Join(base, "mirrors")
		if err := privateTree(mirrors); err != nil {
			return err
		}
		path := filepath.Join(mirrors, snapshot)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			stage, err := os.MkdirTemp(mirrors, "mirror-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(stage)
			if err := copyTree(input, stage); err != nil {
				return err
			}
			if err := os.Rename(stage, path); err != nil && !os.IsExist(err) {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(base, "latest"), []byte(snapshot+"\n")); err != nil {
			return err
		}
		entries, err := os.ReadDir(mirrors)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.Name() == snapshot || !entry.IsDir() {
				continue
			}
			if requireSnapshot(entry.Name()) != nil {
				if !strings.HasPrefix(entry.Name(), "mirror-") {
					continue
				}
				info, err := entry.Info()
				if err != nil || time.Since(info.ModTime()) < 7*24*time.Hour {
					continue
				}
			}
			if err := os.RemoveAll(filepath.Join(mirrors, entry.Name())); err != nil {
				return errors.New("could not reclaim old worker mirror")
			}
		}
		return nil
	})
}

func gitWithIndex(index, gitDir, worktree string, args ...string) ([]byte, error) {
	all := append([]string{"--git-dir=" + gitDir, "--work-tree=" + worktree}, args...)
	cmd := exec.Command("git", all...)
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	result, err := cmd.Output()
	if err != nil {
		return nil, errors.New("could not collect worker edits")
	}
	return result, nil
}

func collectPatch(original, checkout, dest string) string {
	var total int64
	if err := filepath.WalkDir(checkout, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(checkout, path)
		if err != nil {
			return err
		}
		if excludedPath(filepath.ToSlash(rel)) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxFileSize {
			return errors.New("worker produced an unsupported edit")
		}
		total += info.Size()
		if total > maxSnapshotSize {
			return errors.New("worker edits exceed the checkout size limit")
		}
		return nil
	}); err != nil {
		return "worker edits include an unsupported file or exceed the size limit"
	}
	gitDir := filepath.Join(filepath.Dir(checkout), "patch.git")
	if _, err := runOutput("git", "init", "--bare", "-q", gitDir); err != nil {
		return "could not prepare patch collection"
	}
	index := filepath.Join(filepath.Dir(checkout), "patch-index")
	if _, err := gitWithIndex(index, gitDir, original, "read-tree", "--empty"); err != nil {
		return "could not prepare patch collection"
	}
	if _, err := gitWithIndex(index, gitDir, original, "add", "--force", "-A", "--", "."); err != nil {
		return "could not stage original snapshot"
	}
	baseline, err := gitWithIndex(index, gitDir, original, "write-tree")
	if err != nil {
		return "could not record original snapshot"
	}
	tree := strings.TrimSpace(string(baseline))
	if _, err := gitWithIndex(index, gitDir, checkout, "read-tree", tree); err != nil {
		return "could not compare worker edits"
	}
	// Git must ignore generated caches and private paths before staging: removing
	// them from the index afterward would still write their bytes to Git objects.
	patterns := make([]string, 0, len(deniedParts)+len(deniedGlobs))
	for part := range deniedParts {
		patterns = append(patterns, "**/"+part)
	}
	for _, glob := range deniedGlobs {
		patterns = append(patterns, "**/"+glob)
	}
	sort.Strings(patterns)
	if err := writeAtomic(filepath.Join(gitDir, "info", "exclude"), []byte(strings.Join(patterns, "\n")+"\n")); err != nil {
		return "could not exclude private worker files"
	}
	if _, err := gitWithIndex(index, gitDir, checkout, "add", "-A", "--", "."); err != nil {
		return "could not stage worker edits"
	}
	paths, err := gitWithIndex(index, gitDir, checkout, "ls-files", "--cached", "-z")
	if err != nil {
		return "could not filter worker edits"
	}
	private := make([]string, 0)
	for _, path := range bytes.Split(paths, []byte{0}) {
		if len(path) > 0 && excludedPath(string(path)) {
			private = append(private, string(path))
		}
	}
	for len(private) > 0 {
		count := len(private)
		if count > 100 {
			count = 100
		}
		args := append([]string{"rm", "--cached", "-q", "-f", "--ignore-unmatch", "--"}, private[:count]...)
		if _, err := gitWithIndex(index, gitDir, checkout, args...); err != nil {
			return "could not exclude private worker files"
		}
		private = private[count:]
	}
	cmd := exec.Command("git", "--git-dir="+gitDir, "--work-tree="+checkout,
		"diff", "--cached", "--binary", "--no-ext-diff", tree)
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+index, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return "could not collect worker patch"
	}
	file, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "could not create worker patch"
	}
	if err := cmd.Start(); err != nil {
		file.Close()
		return "could not collect worker patch"
	}
	written, copyErr := io.Copy(file, io.LimitReader(pipe, maxPatchSize+1))
	if written > maxPatchSize || copyErr != nil {
		cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	closeErr := file.Close()
	if written > maxPatchSize || copyErr != nil || waitErr != nil || closeErr != nil {
		os.Remove(dest)
		return "worker edits exceed the patch limit or cannot be exported"
	}
	return ""
}

func runWorker(root, action, project, job, snapshot, cwd string, argv, environment []string, stdin io.Reader) (int, error) {
	switch action {
	case "init":
		return 0, initWorker(root)
	case "ready":
		return 0, workerReady(root)
	case "load":
		active, err := workerLoad(root)
		if err == nil {
			fmt.Println(active)
		}
		return 0, err
	case "prepare":
		return 0, workerPrepare(root, project, job)
	case "execute":
		return workerExecute(root, project, job, snapshot, cwd, argv, environment, stdin)
	case "image":
		if project != "" || job != "" || cwd != "." || len(argv) != 0 {
			return 2, errors.New("invalid worker image request")
		}
		return 0, saveImage(root, "sha256:"+snapshot)
	case "status", "cancel":
		var record jobRecord
		var err error
		if action == "status" {
			record, err = workerStatus(root, project, job)
		} else {
			record, err = workerCancel(root, project, job)
		}
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(record)
		}
		return 0, err
	case "logs", "patch":
		name := "output.log"
		if action == "patch" {
			name = "changes.patch"
		}
		content, err := workerFile(root, project, job, name)
		if err == nil {
			_, err = os.Stdout.Write(content)
		}
		return 0, err
	default:
		return 2, errors.New("unknown worker action")
	}
}
