package switchyard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAWSWorkerLeasesAreExclusivePerSlot(t *testing.T) {
	dir := t.TempDir()
	first, err := awsTryLease(dir, "aws")
	if err != nil || first == nil {
		t.Fatalf("claim first worker: %v", err)
	}
	second, err := awsTryLease(dir, "aws")
	if err != nil || second != nil {
		t.Fatalf("busy worker should not be claimed: %v, %v", second, err)
	}
	extra, err := awsTryLease(dir, "aws-2")
	if err != nil || extra == nil {
		t.Fatalf("different worker must remain available: %v", err)
	}
	if err := extra.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err = awsTryLease(dir, "aws")
	if err != nil || second == nil {
		t.Fatalf("released worker should become available: %v", err)
	}
	second.Close()
}

func TestAWSAutoEnvironmentResolvesPrivateRuntime(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	original := []string{"PATH=/test/bin", "XDG_RUNTIME_DIR=", "UNRELATED=preserved"}
	resolved := awsAutoEnvironment(original, dir)
	if strings.Join(resolved, "\n") != "PATH=/test/bin\nUNRELATED=preserved\nXDG_RUNTIME_DIR="+dir {
		t.Fatalf("missing private user-bus environment: %v", resolved)
	}
	if original[1] != "XDG_RUNTIME_DIR=" || len(original) != 3 {
		t.Fatal("resolving the bus changed the invocation environment")
	}
	for _, explicit := range []string{"XDG_RUNTIME_DIR=/chosen/runtime", "DBUS_SESSION_BUS_ADDRESS=unix:path=/chosen/bus"} {
		configured := []string{"PATH=/test/bin", explicit}
		if !equalPaths(awsAutoEnvironment(configured, dir), configured) {
			t.Fatalf("explicit bus environment was replaced: %s", explicit)
		}
	}
}

func TestAWSAutoEnvironmentRejectsUnsafeRuntime(t *testing.T) {
	private := t.TempDir()
	shared := t.TempDir()
	if err := os.Chmod(shared, 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "runtime-link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(private, "regular-file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	original := []string{"PATH=/test/bin"}
	for _, dir := range []string{shared, link, file, filepath.Join(private, "missing")} {
		if !equalPaths(awsAutoEnvironment(original, dir), original) {
			t.Fatal("inferred a bus from an unsafe runtime directory")
		}
	}
}

func TestAutomaticAWSPlacementKeepsUnavailableToolchainsNative(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "aws-worker")
	t.Setenv("SWITCHYARD_HOME", home)
	t.Setenv("SWITCHYARD_AWS_HOME", dir)
	if err := privateTree(dir); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(awsAutoConfig{Source: home, MaxWorkers: 1, IdleAfter: int(awsIdleWindow.Seconds())})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(awsAutoPath(dir), config); err != nil {
		t.Fatal(err)
	}
	tools := filepath.Join(home, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	readyMarker, wakeMarker := filepath.Join(home, "ready-called"), filepath.Join(home, "wake-called")
	adapter := filepath.Join(tools, "worker-adapter")
	if err := os.WriteFile(adapter, []byte(fmt.Sprintf("#!/bin/sh\n: > %q\nexit 1\n", readyMarker)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tools, "systemctl"), []byte(fmt.Sprintf("#!/bin/sh\n: > %q\n", wakeMarker)), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+":"+os.Getenv("PATH"))
	if err := saveBackend(home, backendConfig{Name: "aws", Kind: "plugin", Root: filepath.Join(home, "managed-worker"), Command: adapter}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"go", "cargo", "pytest", "uv", "make"} {
		request := placementCommand{Directory: home, Argv: []string{tool}}
		provider, _, err := selectProviderAtPressure(context.Background(), home, os.Args[0], []string{"cpu"}, true, "CPU contention", request)
		if err != nil || provider.Name() != "local" {
			t.Fatalf("unsupported managed-worker tool did not stay local: %s, %v", tool, err)
		}
		if name := pressureReadyBackendAtPressure(home, os.Args[0], true, request); name != "" {
			t.Fatalf("traced unsupported managed-worker tool selected a remote: %s", tool)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "package.json"), []byte(`{"packageManager":"pnpm@10.27.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request := placementCommand{Directory: home, Argv: []string{"pnpm", "test"}}
	if _, _, err := selectProviderAtPressure(context.Background(), home, os.Args[0], []string{"cpu"}, true, "CPU contention", request); err != nil {
		t.Fatal(err)
	}
	if name := pressureReadyBackendAtPressure(home, os.Args[0], true, request); name != "" {
		t.Fatal("an incompatible pnpm pin selected the managed worker")
	}
	for _, marker := range []string{readyMarker, wakeMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatal("unsupported toolchain probed or woke the managed Node worker")
		}
	}
	if _, err := os.Stat(awsActivityPath(dir)); !os.IsNotExist(err) {
		t.Fatal("unsupported toolchain recorded AWS demand")
	}
	if err := os.WriteFile(filepath.Join(home, "package.json"), []byte(`{"packageManager":"pnpm@10.28.0"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectProviderAtPressure(context.Background(), home, os.Args[0], []string{"cpu"}, true, "CPU contention", request); err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{readyMarker, wakeMarker} {
		if _, err := os.Stat(marker); err != nil {
			t.Fatal("supported Node command did not probe readiness and queue wake")
		}
	}
}

func TestAWSActivityUpdatesPreserveConcurrentDemand(t *testing.T) {
	dir := t.TempDir()
	const count = 20
	var group sync.WaitGroup
	group.Add(count)
	errors := make(chan error, count)
	for range count {
		go func() {
			defer group.Done()
			errors <- awsWithActivity(dir, func(activity *awsAutoActivity) { activity.LastUse++ })
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	content, err := privateRead(awsActivityPath(dir), 4096)
	if err != nil {
		t.Fatal(err)
	}
	var activity awsAutoActivity
	if err := json.Unmarshal(content, &activity); err != nil || activity.LastUse != count {
		t.Fatalf("concurrent updates lost: %v, %+v", err, activity)
	}
}

func TestAWSIdleStopChecksLocalTimeAndRemoteLoad(t *testing.T) {
	now := time.Unix(100_000, 0)
	window := 15 * time.Minute
	for _, item := range []struct {
		name string
		last int64
		load int
		want bool
	}{
		{"never used", 0, 0, false},
		{"recently used", now.Add(-window + time.Second).Unix(), 0, false},
		{"idle", now.Add(-window).Unix(), 0, true},
		{"prepared or active job", now.Add(-2 * window).Unix(), 1, false},
	} {
		t.Run(item.name, func(t *testing.T) {
			if got := awsShouldStop(awsAutoActivity{LastUse: item.last}, now, window, item.load); got != item.want {
				t.Fatalf("stop = %v, want %v", got, item.want)
			}
		})
	}
}

func TestAWSPoolRecoveryRejectsForeignSlot(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	state := awsState{Slot: 2, LaunchToken: "private-token"}
	instance := awsInstance{InstanceID: "i-aabbccddee", Tags: append(append([]awsTag{}, awsTags...),
		awsTag{Key: "Slot", Value: "2"}, awsTag{Key: "LaunchToken", Value: "different-token"})}
	instance.State.Name = "running"
	response := struct {
		Reservations []struct{ Instances []awsInstance }
	}{Reservations: []struct{ Instances []awsInstance }{{Instances: []awsInstance{instance}}}}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join(dir, "response.json")
	if err := os.WriteFile(fixture, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake AWS CLI returns only an isolated, synthetic DescribeInstances result.
	script := filepath.Join(bin, "aws")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /bin/cat '"+fixture+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SWITCHYARD_AWS_HOME", filepath.Join(dir, "aws-home"))
	if _, err := awsFindPoolInstance(state); err == nil || !strings.Contains(err.Error(), "invalid tags") {
		t.Fatalf("foreign instance must be rejected, got %v", err)
	}
	response.Reservations[0].Instances[0].Tags[len(instance.Tags)-1].Value = state.LaunchToken
	data, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture, data, 0o600); err != nil {
		t.Fatal(err)
	}
	found, err := awsFindPoolInstance(state)
	if err != nil || found == nil || found.InstanceID != instance.InstanceID {
		t.Fatalf("owned interrupted launch must be found: %+v, %v", found, err)
	}
}

func TestAWSAutoUnitUsesSelectedStateDirectories(t *testing.T) {
	unit := awsAutoUnit("/opt/switchyard", "/home/user/state", "/home/user/worker", "tick")
	for _, setting := range []string{
		"Environment=SWITCHYARD_HOME=/home/user/state\n",
		"Environment=SWITCHYARD_AWS_HOME=/home/user/worker\n",
		"ExecStart=/opt/switchyard aws auto tick\n",
	} {
		if !strings.Contains(unit, setting) {
			t.Fatalf("service missing %q", setting)
		}
	}
}
