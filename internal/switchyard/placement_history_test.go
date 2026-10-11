package switchyard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPlacementLearnsExactCommandAndDirectoryWithoutStoringArguments(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	secret := "private-argument-sentinel"
	key, ok := commandSignature(project, "/usr/bin/node", []string{"build.js", "--token=" + secret})
	if !ok {
		t.Fatal("valid command was not identified")
	}
	if backend := preferredObservedBackend(home, key, []string{"aws"}); backend != "" {
		t.Fatalf("unknown command was routed to %q", backend)
	}
	for range 2 {
		if err := recordCommandObservation(home, key, "local", 12*time.Second, 8*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if backend := preferredObservedBackend(home, key, []string{"aws"}); backend != "" {
		t.Fatalf("command without remote evidence was routed to %q", backend)
	}
	for range 2 {
		if err := recordCommandObservation(home, key, "aws", 4*time.Second, 0); err != nil {
			t.Fatal(err)
		}
	}
	if backend := preferredObservedBackend(home, key, []string{"aws"}); backend != "aws" {
		t.Fatalf("measured faster backend was not selected: %q", backend)
	}
	if backend := preferredObservedBackend(home, key, []string{"unavailable"}); backend != "" {
		t.Fatalf("unavailable backend was selected: %q", backend)
	}
	noArgs, _ := commandSignature(project, "/usr/bin/node", nil)
	emptyArg, _ := commandSignature(project, "/usr/bin/node", []string{""})
	if noArgs == emptyArg {
		t.Fatal("empty argument collided with no arguments")
	}
	for _, changed := range []struct {
		cwd  string
		args []string
	}{
		{filepath.Join(project, "subdir"), []string{"build.js", "--token=" + secret}},
		{project, []string{"build.js", "--token=other"}},
	} {
		other, ok := commandSignature(changed.cwd, "/usr/bin/node", changed.args)
		if !ok || other == key || preferredObservedBackend(home, other, []string{"aws"}) != "" {
			t.Fatal("observations crossed an invocation boundary")
		}
	}
	data, err := os.ReadFile(filepath.Join(home, "observations", "timings.json"))
	if err != nil || strings.Contains(string(data), secret) || strings.Contains(string(data), project) {
		t.Fatal("stored history disclosed a command argument or directory")
	}
	info, err := os.Stat(filepath.Join(home, "observations", "timings.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("observation history was not private")
	}
}

func TestStatisticsUsePrivateObservationsWithoutDisplayingCommands(t *testing.T) {
	home := t.TempDir()
	var output bytes.Buffer
	if err := writeStatistics(home, nil, &output, 10); err != nil || !strings.Contains(output.String(), "Commands observed: 0") {
		t.Fatalf("empty statistics were not readable: %v %q", err, output.String())
	}
	secret := "secret-argument-sentinel"
	project := t.TempDir()
	key, ok := commandSignature(project, "/usr/bin/node", []string{"--token=" + secret})
	if !ok {
		t.Fatal("valid command was not identified")
	}
	provider := shellProvider{cfg: backendConfig{Name: "aws", Kind: "ssh", Root: "/private/worker", Host: "example.invalid", Identity: "/private/key"}}
	remote := observedBackendID(provider)
	for _, duration := range []time.Duration{10 * time.Second, 12 * time.Second} {
		if err := recordCommandObservation(home, key, "local", duration, time.Second); err != nil {
			t.Fatal(err)
		}
	}
	for _, duration := range []time.Duration{2 * time.Second, 4 * time.Second} {
		if err := recordCommandObservation(home, key, remote, duration, 0); err != nil {
			t.Fatal(err)
		}
	}
	output.Reset()
	if err := writeStatistics(home, []shellProvider{provider}, &output, 10); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, key[:12]) || !strings.Contains(text, "Commands observed: 1") || !strings.Contains(text, "local") || !strings.Contains(text, "aws") || !strings.Contains(text, "11000") || !strings.Contains(text, "3000") {
		t.Fatalf("statistics omitted the recorded samples: %s", text)
	}
	if strings.Contains(text, secret) || strings.Contains(text, project) || strings.Contains(text, "/private/key") {
		t.Fatal("statistics disclosed an argument, directory or backend identity")
	}
	if err := os.Chmod(observationFile(home), 0o644); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := writeStatistics(home, nil, &output, 10); err == nil || output.Len() != 0 {
		t.Fatal("statistics read an untrusted observation file")
	}
}

func TestPlacementDoesNotRouteWhenRemoteIsSlower(t *testing.T) {
	home := t.TempDir()
	key, ok := commandSignature(t.TempDir(), "/bin/true", nil)
	if !ok {
		t.Fatal("valid no-argument command was rejected")
	}
	for range 2 {
		if err := recordCommandObservation(home, key, "local", 2*time.Second, time.Second); err != nil {
			t.Fatal(err)
		}
		if err := recordCommandObservation(home, key, "aws", 6*time.Second, 0); err != nil {
			t.Fatal(err)
		}
	}
	if backend := preferredObservedBackend(home, key, []string{"aws"}); backend != "" {
		t.Fatalf("slower remote backend was selected: %q", backend)
	}
}

func TestPlacementRequiresMeasuredMargin(t *testing.T) {
	home := t.TempDir()
	key, ok := commandSignature(t.TempDir(), "/bin/true", nil)
	if !ok {
		t.Fatal("valid command was rejected")
	}
	for range 2 {
		if err := recordCommandObservation(home, key, "local", 200*time.Millisecond, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		if err := recordCommandObservation(home, key, "aws", 190*time.Millisecond, 0); err != nil {
			t.Fatal(err)
		}
	}
	if backend := preferredObservedBackend(home, key, []string{"aws"}); backend != "" {
		t.Fatalf("noise-scale speedup was treated as a gain: %q", backend)
	}
}

func TestObservationRejectsRoundedValuesThatWouldPoisonHistory(t *testing.T) {
	home := t.TempDir()
	key, ok := commandSignature(t.TempDir(), "/bin/true", nil)
	if !ok {
		t.Fatal("valid command was rejected")
	}
	// These durations pass a nanosecond comparison, but truncation to whole
	// milliseconds makes the stored CPU time exceed 256 times the wall time.
	if err := recordCommandObservation(home, key, "local", 3*time.Millisecond+10*time.Microsecond, 770*time.Millisecond); err == nil {
		t.Fatal("observation stored CPU and wall values that cannot be read back")
	}
	history, err := readObservations(home)
	if err != nil || len(history) != 0 {
		t.Fatalf("rejected observation corrupted history: %v", err)
	}
	if err := recordCommandObservation(home, key, "local", 12*time.Hour, time.Second); err != nil {
		t.Fatalf("long valid duration was rejected: %v", err)
	}
	history, err = readObservations(home)
	if err != nil || len(history[key].Backends["local"]) != 1 {
		t.Fatalf("long observation could not be read back: %v", err)
	}
}
