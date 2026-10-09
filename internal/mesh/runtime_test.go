package mesh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxRuntimeSelectsPodmanWithoutDocker(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("MESH_CONTAINER_RUNTIME", "")
	if engine, err := containerRuntime(); err != nil || engine != "podman" {
		t.Fatalf("daemonless runtime was not selected: %q %v", engine, err)
	}
	t.Setenv("MESH_CONTAINER_RUNTIME", "docker")
	if _, err := containerRuntime(); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("explicit Docker choice must not silently use Podman: %v", err)
	}
	t.Setenv("MESH_CONTAINER_RUNTIME", "something-else")
	if _, err := containerRuntime(); err == nil || !strings.Contains(err.Error(), "must be podman or docker") {
		t.Fatalf("unknown container engine must fail closed: %v", err)
	}
}

func TestWorkerImageStaysInItsOriginalRuntime(t *testing.T) {
	bin := t.TempDir()
	image := "sha256:" + strings.Repeat("a", 64)
	for _, name := range []string{"podman", "docker"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("MESH_CONTAINER_RUNTIME", "podman")
	root := filepath.Join(t.TempDir(), "worker")
	if err := saveImage(root, image); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MESH_CONTAINER_RUNTIME", "docker")
	if _, err := loadImage(root); err == nil {
		t.Fatal("an explicit runtime change must not silently run an image built in Podman")
	}
	t.Setenv("MESH_CONTAINER_RUNTIME", "")
	config, err := loadImage(root)
	if err != nil || config.Runtime != "podman" || config.ID != image {
		t.Fatalf("worker image silently switched container storage: %+v %v", config, err)
	}
	if err := os.Remove(filepath.Join(bin, "podman")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadImage(root); err == nil {
		t.Fatal("a worker must not run an image ID from another runtime's storage")
	}
}
