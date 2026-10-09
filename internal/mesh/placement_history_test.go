package mesh

import (
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
