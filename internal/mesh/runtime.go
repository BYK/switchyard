package mesh

import (
	"errors"
	"os"
	"os/exec"
)

// Linux workers use Podman's local, daemonless runtime when it is installed.
// Docker remains available for existing workers and local development.
func containerRuntime() (string, error) {
	selected := os.Getenv("MESH_CONTAINER_RUNTIME")
	if selected != "" && selected != "podman" && selected != "docker" {
		return "", errors.New("MESH_CONTAINER_RUNTIME must be podman or docker")
	}
	if selected != "" {
		if _, err := exec.LookPath(selected); err != nil {
			return "", errors.New("selected container runtime is not installed")
		}
		return selected, nil
	}
	for _, name := range []string{"podman", "docker"} {
		if _, err := exec.LookPath(name); err == nil {
			return name, nil
		}
	}
	return "", errors.New("worker needs rootless Podman or an explicitly installed Docker runtime")
}
