package switchyard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const awsIdleWindow = 15 * time.Minute

func isAWSWorker(name string) bool {
	return name == "aws" || name == "aws-2" || name == "aws-3" || name == "aws-4"
}

func awsWorkerAvailable(name string) (bool, error) {
	if !isAWSWorker(name) {
		return false, errors.New("invalid managed worker")
	}
	dir, err := awsDirectory()
	if err != nil {
		return false, err
	}
	lease, err := awsTryLease(dir, name)
	if err != nil || lease == nil {
		return false, err
	}
	if err := lease.Close(); err != nil {
		return false, err
	}
	return true, nil
}

type awsAutoConfig struct {
	Source     string `json:"source"`
	MaxWorkers int    `json:"max_workers"`
	IdleAfter  int    `json:"idle_after_seconds"`
}

type awsAutoActivity struct {
	LastUse int64            `json:"last_use"`
	Demand  int64            `json:"demand"`
	Desired int              `json:"desired,omitempty"`
	Workers map[string]int64 `json:"workers,omitempty"`
}

func awsAutoPath(dir string) string     { return filepath.Join(dir, "autoscale.json") }
func awsActivityPath(dir string) string { return filepath.Join(dir, "activity.json") }

func awsAutoRead(dir string) (awsAutoConfig, bool, error) {
	if _, err := os.Lstat(awsAutoPath(dir)); os.IsNotExist(err) {
		return awsAutoConfig{}, false, nil
	} else if err != nil {
		return awsAutoConfig{}, false, errors.New("could not inspect AWS autoscale configuration")
	}
	content, err := privateRead(awsAutoPath(dir), 4096)
	if err != nil {
		return awsAutoConfig{}, false, errors.New("AWS autoscale configuration is not private")
	}
	var config awsAutoConfig
	if json.Unmarshal(content, &config) != nil || !filepath.IsAbs(config.Source) || config.MaxWorkers < 1 || config.MaxWorkers > 4 || config.IdleAfter != int(awsIdleWindow.Seconds()) {
		return awsAutoConfig{}, false, errors.New("AWS autoscale configuration is invalid")
	}
	return config, true, nil
}

func awsReadActivity(dir string) (awsAutoActivity, error) {
	if _, err := os.Lstat(awsActivityPath(dir)); os.IsNotExist(err) {
		return awsAutoActivity{}, nil
	} else if err != nil {
		return awsAutoActivity{}, errors.New("could not inspect worker activity")
	}
	content, err := privateRead(awsActivityPath(dir), 4096)
	if err != nil {
		return awsAutoActivity{}, err
	}
	var activity awsAutoActivity
	if json.Unmarshal(content, &activity) != nil || activity.LastUse < 0 || activity.Demand < 0 || activity.Desired < 0 || activity.Desired > 4 {
		return awsAutoActivity{}, errors.New("invalid worker activity")
	}
	for name, last := range activity.Workers {
		if (name != "aws" && name != "aws-2" && name != "aws-3" && name != "aws-4") || last < 0 {
			return awsAutoActivity{}, errors.New("invalid worker activity")
		}
	}
	return activity, nil
}

// Hold the same lock across the entire job, or across an idle check and stop.
// A second dispatcher must choose a different worker or stay local.
func awsTryLease(dir, name string) (*os.File, error) {
	if err := privateTree(dir); err != nil {
		return nil, err
	}
	if !remoteName.MatchString(name) {
		return nil, errors.New("invalid worker lease name")
	}
	fd, err := syscall.Open(filepath.Join(dir, ".lease-"+name), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, errors.New("could not open worker lease")
	}
	file := os.NewFile(uintptr(fd), "worker lease")
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		file.Close()
		return nil, errors.New("worker lease is not private")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, errors.New("could not lock worker lease")
	}
	return file, nil
}

func awsWithActivity(dir string, update func(*awsAutoActivity)) error {
	if err := privateTree(dir); err != nil {
		return err
	}
	lease, err := awsTryLease(dir, "activity")
	if err != nil {
		return err
	}
	if lease == nil {
		// Wait for a concurrent writer rather than losing its update.
		fd, err := syscall.Open(filepath.Join(dir, ".lease-activity"), syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return errors.New("could not open worker activity lock")
		}
		lease = os.NewFile(uintptr(fd), "worker activity lock")
		if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
			lease.Close()
			return errors.New("could not lock worker activity")
		}
	}
	defer lease.Close()
	activity, err := awsReadActivity(dir)
	if err != nil {
		return err
	}
	update(&activity)
	content, err := json.Marshal(activity)
	if err != nil {
		return err
	}
	return writeAtomic(awsActivityPath(dir), append(content, '\n'))
}

func awsNoteActivity(dir string, demand bool) error {
	return awsWithActivity(dir, func(activity *awsAutoActivity) {
		activity.LastUse = time.Now().Unix()
		if demand {
			activity.Demand = activity.LastUse
			if activity.Desired == 0 {
				activity.Desired = 1
			}
		}
	})
}

func awsNoteWorkerUse(dir, name string) error {
	if name != "aws" && name != "aws-2" && name != "aws-3" && name != "aws-4" {
		return errors.New("invalid AWS worker name")
	}
	return awsWithActivity(dir, func(activity *awsAutoActivity) {
		if activity.Workers == nil {
			activity.Workers = make(map[string]int64)
		}
		activity.Workers[name] = time.Now().Unix()
	})
}

// Demand recording never uses AWS credentials and never replaces a tool result.
func awsRequestWake() {
	awsRequestCapacity(false)
}

func awsRequestScale() {
	awsRequestCapacity(true)
}

func awsRequestCapacity(scale bool) {
	dir, err := awsDirectory()
	if err != nil {
		return
	}
	config, enabled, err := awsAutoRead(dir)
	if err != nil || !enabled {
		return
	}
	if err := awsWithActivity(dir, func(activity *awsAutoActivity) {
		now := time.Now().Unix()
		if activity.Desired == 0 || time.Unix(activity.Demand, 0).Add(awsIdleWindow).Before(time.Unix(now, 0)) {
			activity.Desired = 1
		}
		if scale && activity.Desired < config.MaxWorkers {
			activity.Desired++
		}
		activity.Demand, activity.LastUse = now, now
	}); err != nil {
		fmt.Fprintln(os.Stderr, "switchyard: could not save AWS wake request")
		return
	}
	// Starting the service queues reconciliation without waiting for cloud APIs.
	cmd := exec.Command("systemctl", "--user", "start", "--no-block", "switchyard-autoscale.service")
	cmd.Stdin = bytes.NewReader(nil)
	if cmd.Run() != nil {
		fmt.Fprintln(os.Stderr, "switchyard: could not start AWS autoscale service")
	}
}

func awsRemoteLoad(dir, ip string) (int, error) {
	remote, err := awsShellTokens(awsCommand, "worker", "--root", awsRoot, "load")
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", append(awsSSHOptions(dir), "ubuntu@"+ip, remote)...)
	cmd.Stdin = bytes.NewReader(nil)
	var output responseBuffer
	output.limit = 32
	cmd.Stdout = &output
	if cmd.Run() != nil || output.tooLarge {
		return 0, errors.New("could not verify active worker jobs")
	}
	value := strings.TrimSpace(output.data.String())
	if value == "" || len(value) > 6 || (len(value) > 1 && value[0] == '0') {
		return 0, errors.New("invalid worker load")
	}
	active, err := strconv.Atoi(value)
	if err != nil || active < 0 || active > 4096 {
		return 0, errors.New("invalid worker load")
	}
	return active, nil
}

func awsShouldStop(activity awsAutoActivity, now time.Time, idle time.Duration, active int) bool {
	return active == 0 && activity.LastUse > 0 && !now.Before(time.Unix(activity.LastUse, 0).Add(idle))
}

func awsAutoTick(home, dir string) error {
	config, enabled, err := awsAutoRead(dir)
	if err != nil || !enabled {
		return err
	}
	lease, err := awsTryLease(dir, "reconcile")
	if err != nil || lease == nil {
		return err
	}
	defer lease.Close()
	activity, err := awsReadActivity(dir)
	if err != nil {
		return err
	}
	for slot := 1; slot <= 4; slot++ {
		workerDir := awsPoolDir(dir, slot)
		eligible := slot <= config.MaxWorkers && slot <= max(1, activity.Desired) && time.Since(time.Unix(activity.Demand, 0)) <= awsIdleWindow
		if err := awsAutoTickSlot(home, dir, workerDir, config, eligible); err != nil {
			return err
		}
	}
	return nil
}

func awsAutoTickSlot(home, base, dir string, config awsAutoConfig, eligible bool) error {
	name := awsBackendName(dir)
	if _, err := os.Lstat(awsStatePath(dir)); os.IsNotExist(err) {
		if !eligible {
			return nil
		}
		if err := awsNoteWorkerUse(base, name); err != nil {
			return err
		}
		return awsUp(home, dir)
	} else if err != nil {
		return errors.New("could not inspect tracked AWS worker")
	}
	state, err := awsLoad(dir)
	if err != nil {
		return err
	}
	if state.InstanceID == "" {
		if state.Slot > 1 && eligible {
			return awsUp(home, dir)
		}
		return errors.New("AWS launch is incomplete; inspect tagged resources")
	}
	info, err := awsInstanceInfo(state)
	if err != nil {
		return err
	}
	switch info.State.Name {
	case "stopped":
		if eligible {
			if err := awsNoteWorkerUse(base, name); err != nil {
				return err
			}
			return awsUp(home, dir)
		}
		return nil
	case "pending", "stopping":
		return nil
	case "running":
	default:
		return errors.New("tracked AWS worker is in an unexpected state")
	}
	trusted, err := awsTrust(dir, state, info)
	if err != nil || !trusted {
		return err
	}
	ip, err := awsAddress(info)
	if err != nil {
		return err
	}
	// Setup can replace the worker binary and image. It must never overlap
	// dispatch, even when this is an older worker without the load marker.
	worker, err := awsTryLease(base, name)
	if err != nil || worker == nil {
		return err
	}
	defer worker.Close()
	if !awsSSH(dir, ip, "test", "-f", "/var/lib/switchyard/ready") {
		return nil
	}
	if !awsSSH(dir, ip, "test", "-x", awsCommand) || !awsSSH(dir, ip, awsCommand, "image", "status") ||
		!awsSSH(dir, ip, "test", "-f", "/home/ubuntu/.local/bin/.switchyard-load-v2") ||
		!awsSSH(dir, ip, "test", "-x", "/usr/local/sbin/switchyard-safe-stop") {
		if err := awsSetup(home, dir, config.Source); err != nil {
			return err
		}
		return awsNoteWorkerUse(base, name)
	}
	if err := awsConfigure(home, dir, info); err != nil {
		return err
	}
	// Re-read under the dispatch lease. A job may have finished since the
	// reconciliation loop loaded activity; stale time would stop it too early.
	activity, err := awsReadActivity(base)
	if err != nil {
		return err
	}
	active, err := awsRemoteLoad(dir, ip)
	last := activity.Workers[name]
	if last == 0 {
		last = max(int64(state.CreatedAt), activity.LastUse)
	}
	if err != nil || !awsShouldStop(awsAutoActivity{LastUse: last}, time.Now(), time.Duration(config.IdleAfter)*time.Second, active) {
		return err
	}
	_, err = awsCall("ec2", "stop-instances", "--instance-ids", state.InstanceID)
	return err
}

func awsWaitReady(ctx context.Context, home, self, name, dir string) error {
	config, enabled, err := awsAutoRead(dir)
	if err != nil || !enabled {
		return errors.New("requested backend is not ready")
	}
	slot := 1
	if name != "aws" {
		if !isAWSWorker(name) {
			return errors.New("invalid managed worker")
		}
		slot = int(name[len(name)-1] - '0')
	}
	if slot > config.MaxWorkers {
		return errors.New("requested worker exceeds the configured pool size")
	}
	if err := awsWithActivity(dir, func(activity *awsAutoActivity) {
		activity.Demand = time.Now().Unix()
		activity.Desired = max(activity.Desired, slot)
	}); err != nil {
		return err
	}
	deadline, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	for {
		if err := awsAutoTick(home, dir); err != nil {
			return err
		}
		current, err := currentBackend(home, self, name)
		if err == nil && current.Ready(deadline) {
			return nil
		}
		select {
		case <-deadline.Done():
			return errors.New("AWS worker did not become ready within eight minutes")
		case <-time.After(10 * time.Second):
		}
	}
}

func awsAutoUnit(self, action string) string {
	return "# Managed by Switchyard AWS autoscale\n[Unit]\nDescription=Reconcile Switchyard AWS workers\n[Service]\nType=oneshot\nTimeoutStartSec=15min\nExecStart=" + self + " aws auto " + action + "\n"
}

const awsAutoTimer = "# Managed by Switchyard AWS autoscale\n[Unit]\nDescription=Check Switchyard AWS worker demand and idle time\n[Timer]\nOnBootSec=1min\nOnUnitInactiveSec=1min\nUnit=switchyard-autoscale.service\n[Install]\nWantedBy=timers.target\n"

func awsInstallAutoUnits(self string) error {
	if !filepath.IsAbs(self) || strings.ContainsAny(self, " \n\r\t\\%'\"") {
		return errors.New("Switchyard executable path contains unsupported systemd unit characters")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for name, content := range map[string]string{
		"switchyard-autoscale.service": awsAutoUnit(self, "tick"),
		"switchyard-autoscale.timer":   awsAutoTimer,
	} {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); err == nil {
			previous, err := privateRead(path, 4096)
			if err != nil || !strings.HasPrefix(string(previous), "# Managed by Switchyard AWS autoscale\n") {
				return errors.New("unrelated autoscale unit already exists")
			}
		} else if !os.IsNotExist(err) {
			return errors.New("could not inspect autoscale unit")
		}
		if err := writeAtomic(path, []byte(content)); err != nil {
			return err
		}
	}
	for _, args := range [][]string{{"--user", "daemon-reload"}, {"--user", "enable", "--now", "switchyard-autoscale.timer"}} {
		command := exec.Command("systemctl", args...)
		command.Stdin = bytes.NewReader(nil)
		if command.Run() != nil {
			return errors.New("could not enable the user autoscale timer; run systemctl --user daemon-reload and systemctl --user enable --now switchyard-autoscale.timer from your login shell")
		}
	}
	return nil
}

func awsConfigureAuto(dir, self, source string, maxWorkers int) error {
	if !filepath.IsAbs(source) || maxWorkers < 1 || maxWorkers > 4 {
		return errors.New("use --source ABSOLUTE_SWITCHYARD_PATH --max-workers 1..4")
	}
	module, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil || !strings.HasPrefix(string(module), "module github.com/BYK/switchyard\n") {
		return errors.New("--source must point to the Switchyard Go source tree")
	}
	if err := privateTree(dir); err != nil {
		return err
	}
	config := awsAutoConfig{Source: source, MaxWorkers: maxWorkers, IdleAfter: int(awsIdleWindow.Seconds())}
	content, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err := writeAtomic(awsAutoPath(dir), append(content, '\n')); err != nil {
		return err
	}
	if err := awsNoteActivity(dir, false); err != nil {
		return err
	}
	return awsInstallAutoUnits(self)
}

func awsDisableAuto(dir string) error {
	cmd := exec.Command("systemctl", "--user", "disable", "--now", "switchyard-autoscale.timer")
	cmd.Stdin = bytes.NewReader(nil)
	if cmd.Run() != nil {
		return errors.New("could not disable the user autoscale timer")
	}
	if err := os.Remove(awsAutoPath(dir)); err != nil && !os.IsNotExist(err) {
		return errors.New("could not remove autoscale configuration")
	}
	return nil
}

func awsResult(err error) int {
	if err != nil {
		return fail(err)
	}
	return 0
}
