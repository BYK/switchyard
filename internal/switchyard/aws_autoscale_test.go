package switchyard

import (
	"encoding/json"
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
