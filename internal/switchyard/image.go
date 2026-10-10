package switchyard

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed node-worker.Dockerfile
var nodeWorkerDockerfile []byte

type workerImage struct {
	ID      string `json:"id"`
	Runtime string `json:"runtime"`
}

func validImageID(id string) bool {
	return strings.HasPrefix(id, "sha256:") && snapshotPattern.MatchString(strings.TrimPrefix(id, "sha256:"))
}

func imageFile(root string) string { return filepath.Join(root, "image.json") }

func inspectImage(id string) error {
	engine, err := containerRuntime()
	if err != nil {
		return err
	}
	return inspectRuntimeImage(engine, id)
}

func inspectRuntimeImage(engine, id string) error {
	if !validImageID(id) {
		return errors.New("worker image needs a full sha256 image ID")
	}
	cmd := exec.Command(engine, "image", "inspect", id)
	if err := cmd.Run(); err != nil {
		if engine == "podman" && exec.Command("podman", "info").Run() != nil {
			return errors.New("rootless Podman is not ready; check user namespaces and subordinate UID/GID ranges")
		}
		return errors.New("pinned Node image is not loaded in the selected container runtime")
	}
	return nil
}

func saveImage(root, id string) error {
	engine, err := containerRuntime()
	if err != nil {
		return err
	}
	if err := inspectRuntimeImage(engine, id); err != nil {
		return err
	}
	if err := privateTree(root); err != nil {
		return err
	}
	content, err := json.Marshal(workerImage{ID: id, Runtime: engine})
	if err != nil {
		return errors.New("could not record worker image")
	}
	return writeAtomic(imageFile(root), append(content, '\n'))
}

func loadImage(root string) (workerImage, error) {
	content, err := privateRead(imageFile(root), 4096)
	if err != nil {
		return workerImage{}, errors.New("worker has no pinned Node image; run switchyard image build")
	}
	var config workerImage
	if json.Unmarshal(content, &config) != nil || !validImageID(config.ID) || config.Runtime != "podman" && config.Runtime != "docker" {
		return workerImage{}, errors.New("worker image configuration is invalid; rebuild it with switchyard image build")
	}
	if requested := os.Getenv("SWITCHYARD_CONTAINER_RUNTIME"); requested != "" && config.Runtime != requested {
		return workerImage{}, errors.New("worker image belongs to another container runtime; rebuild it with switchyard image build")
	}
	if err := inspectRuntimeImage(config.Runtime, config.ID); err != nil {
		return workerImage{}, err
	}
	return config, nil
}

func buildNodeImage(home string) (string, error) {
	engine, err := containerRuntime()
	if err != nil {
		return "", err
	}
	base, _, ok := strings.Cut(string(nodeWorkerDockerfile), "\n")
	base = strings.TrimPrefix(base, "FROM ")
	if !ok || !containerImagePattern.MatchString(base) {
		return "", errors.New("Node worker base image is not digest-pinned")
	}
	if err := exec.Command(engine, "image", "inspect", base).Run(); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := exec.CommandContext(ctx, engine, "pull", base).Run(); err != nil {
			return "", errors.New("could not pull the pinned Node base image")
		}
	}
	var cmd *exec.Cmd
	if engine == "podman" {
		// An empty context prevents accidentally copying the invoking project into
		// the image. Podman accepts a Containerfile from stdin with -f -.
		contextDir, err := os.MkdirTemp("", "switchyard-node-image-*")
		if err != nil {
			return "", errors.New("could not prepare Node image build context")
		}
		defer os.RemoveAll(contextDir)
		cmd = exec.Command("podman", "build", "--pull=never", "--platform", "linux/amd64", "--quiet", "-f", "-", contextDir)
	} else {
		cmd = exec.Command("docker", "build", "--pull=false", "--platform", "linux/amd64", "--quiet", "-")
	}
	cmd.Stdin = bytes.NewReader(nodeWorkerDockerfile)
	var output responseBuffer
	output.limit = 1024
	cmd.Stdout = &output
	started := time.Now()
	if err := cmd.Run(); err != nil || output.tooLarge {
		debugEvent("image-build", "error", started)
		return "", errors.New("could not build the pinned Node image")
	}
	debugEvent("image-build", "ok", started)
	id := strings.TrimSpace(output.data.String())
	if snapshotPattern.MatchString(id) {
		id = "sha256:" + id
	}
	if err := saveImage(filepath.Join(home, "images"), id); err != nil {
		return "", err
	}
	return id, nil
}

func runImage(home string, args []string) int {
	if len(args) != 1 {
		return fail(errors.New("use image build or image status"))
	}
	root := filepath.Join(home, "images")
	switch args[0] {
	case "build":
		id, err := buildNodeImage(home)
		if err != nil {
			return fail(err)
		}
		if err := initWorker(filepath.Join(home, "worker")); err != nil {
			return fail(err)
		}
		if err := saveImage(filepath.Join(home, "worker"), id); err != nil {
			return fail(err)
		}
		fmt.Println("Pinned Node worker image ready:", id)
		return 0
	case "status":
		id, err := loadImage(root)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("Pinned Node worker image (%s): %s\n", id.Runtime, id.ID)
		return 0
	default:
		return fail(errors.New("use image build or image status"))
	}
}
