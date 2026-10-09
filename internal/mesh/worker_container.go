package mesh

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func containerJobEnvironment(environment []string, target string) []string {
	result := make([]string, 0, len(environment)+5)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if name != "HOME" && name != "PATH" && name != "PWD" && name != "TMPDIR" && name != "XDG_CACHE_HOME" && name != "XDG_CONFIG_HOME" && name != "XDG_DATA_HOME" && name != "XDG_RUNTIME_DIR" && name != "MESH_BACKEND" && name != "MESH_CONTAINER_RUNTIME" {
			result = append(result, entry)
		}
	}
	return append(result, "HOME=/home/mesh", "PATH=/usr/local/bin:/usr/bin:/bin", "TMPDIR=/tmp",
		"XDG_CACHE_HOME=/home/mesh/.cache", "XDG_CONFIG_HOME=/home/mesh/.config",
		"XDG_DATA_HOME=/home/mesh/.local/share", "PWD="+target, "MESH_BACKEND=local")
}

// Container engines must use the worker account's runtime state, not the
// invoking machine's HOME, runtime socket or container-client configuration.
// Fixed, non-secret container paths are passed explicitly to the engine.
func containerEngineEnvironment(environment []string) ([]string, error) {
	result := make([]string, 0, len(environment)+len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !containerEngineControl(name) {
			result = append(result, entry)
		}
	}
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if name == "MESH_CONTAINER_RUNTIME" {
			continue
		}
		if containerEngineControl(name) {
			return nil, errors.New("invocation environment cannot override container engine configuration")
		}
		if name != "HOME" && name != "PATH" && name != "PWD" && name != "TMPDIR" && name != "XDG_CACHE_HOME" && name != "XDG_CONFIG_HOME" && name != "XDG_DATA_HOME" && name != "XDG_RUNTIME_DIR" && name != "MESH_BACKEND" && name != "USER" && name != "LOGNAME" {
			result = append(result, entry)
		}
	}
	return result, nil
}

func containerEngineControl(name string) bool {
	return strings.HasPrefix(name, "DOCKER_") || strings.HasPrefix(name, "CONTAINER_") ||
		strings.HasPrefix(name, "CONTAINERS_") || strings.HasPrefix(name, "BUILDAH_") || strings.HasPrefix(name, "PODMAN_") ||
		strings.HasPrefix(name, "STORAGE_") ||
		name == "REGISTRY_AUTH_FILE" || name == "MESH_CONTAINER_RUNTIME"
}

func containerPackageRoot(checkout, target, lock string) (string, error) {
	for directory := target; ; directory = filepath.Dir(directory) {
		packageInfo, packageErr := os.Lstat(filepath.Join(directory, "package.json"))
		lockInfo, lockErr := os.Lstat(filepath.Join(directory, lock))
		if packageErr == nil && lockErr == nil && packageInfo.Mode().IsRegular() && lockInfo.Mode().IsRegular() {
			relative, err := filepath.Rel(checkout, directory)
			if err != nil {
				return "", errors.New("could not locate the Node package in the job checkout")
			}
			return filepath.Join("/workspace", relative), nil
		}
		if directory == checkout {
			break
		}
	}
	return "", errors.New("Node dependency setup needs a package.json and matching lockfile in the snapshot")
}

func validatePinnedPnpm(packageRoot string) error {
	file, err := os.Open(filepath.Join(packageRoot, "package.json"))
	if err != nil {
		return errors.New("could not inspect Node package manager version")
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, 256<<10+1))
	if err != nil || len(content) > 256<<10 {
		return errors.New("Node package metadata is too large")
	}
	var info struct {
		PackageManager string `json:"packageManager"`
	}
	if json.Unmarshal(content, &info) != nil || strings.SplitN(info.PackageManager, "+", 2)[0] != "pnpm@10.28.0" {
		return errors.New("Node package must pin pnpm@10.28.0 for this worker image")
	}
	return nil
}

func containerJobCommand(root, jobPath, checkout, cwd string, argv, environment []string, image workerImage) (*exec.Cmd, bool, error) {
	if err := inspectRuntimeImage(image.Runtime, image.ID); err != nil {
		return nil, false, err
	}
	if strings.ContainsAny(checkout, ",\n") || strings.ContainsAny(jobPath, ",\n") || strings.ContainsAny(root, ",\n") {
		return nil, false, errors.New("Docker bind paths cannot contain commas or newlines")
	}
	cache := filepath.Join(root, "caches", "node")
	setupDir := filepath.Join(jobPath, "setup")
	if err := privateTree(cache); err != nil {
		return nil, false, err
	}
	if err := privateTree(setupDir); err != nil {
		return nil, false, err
	}
	target := filepath.Join("/workspace", cwd)
	containerArgs := append([]string{}, argv...)
	manager := filepath.Base(argv[0])
	setup := manager == "pnpm" || manager == "npm"
	if setup {
		lock := "pnpm-lock.yaml"
		if manager == "npm" {
			lock = "package-lock.json"
		}
		packageRoot, err := containerPackageRoot(checkout, filepath.Join(checkout, cwd), lock)
		if err != nil {
			return nil, true, err
		}
		if manager == "pnpm" {
			relative, err := filepath.Rel("/workspace", packageRoot)
			if err != nil {
				return nil, true, errors.New("could not locate the Node package in the job checkout")
			}
			if err := validatePinnedPnpm(filepath.Join(checkout, relative)); err != nil {
				return nil, true, err
			}
		}
		// The marker lives outside the mounted worktree and is written only after
		// locked dependency setup succeeds. The job's real command then execs.
		script := `cd "$1" || exit 2
if [ "$2" = pnpm ]; then
  pnpm install --frozen-lockfile --prefer-offline || exit $?
else
  npm ci --prefer-offline --no-audit --no-fund || exit $?
fi
cd "$4" || exit 2
: > "$3" || exit 2
shift 4
exec "$@"`
		containerArgs[0] = manager
		containerArgs = append([]string{"sh", "-c", script, "mesh-node-setup", packageRoot,
			manager, "/mesh-setup/ready", target}, containerArgs...)
	}
	network := "none"
	if setup {
		network = "bridge"
	}
	if override := os.Getenv("MESH_WORKER_NETWORK"); override != "" {
		if override != "none" && override != "bridge" {
			return nil, setup, errors.New("MESH_WORKER_NETWORK must be none or bridge")
		}
		network = override
	}
	if image.Runtime == "podman" && network == "bridge" {
		// Rootless Podman uses a private userspace network, never the host network.
		network = "pasta"
	}
	args := []string{"run", "--rm", "--interactive", "--init", "--pull=never", "--network=" + network,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--pids-limit=512",
		"--cidfile", filepath.Join(jobPath, "container.cid"),
		"--label", "mesh.job=" + filepath.Base(jobPath),
		"--label", "mesh.project=" + filepath.Base(filepath.Dir(filepath.Dir(jobPath))),
		"--mount", "type=bind,source=" + checkout + ",target=/workspace",
		"--mount", "type=bind,source=" + cache + ",target=/home/mesh",
		"--mount", "type=bind,source=" + setupDir + ",target=/mesh-setup",
		"--workdir", target}
	if image.Runtime == "podman" {
		args = append(args, "--userns=keep-id", "--http-proxy=false")
	}
	containerEnv := containerJobEnvironment(environment, target)
	identityEnv := make([]string, 0, 2)
	for _, entry := range containerEnv {
		name, _, _ := strings.Cut(entry, "=")
		if name == "USER" || name == "LOGNAME" {
			if strings.ContainsAny(entry, "\r\n") {
				return nil, setup, errors.New("container identity environment contains a newline")
			}
			identityEnv = append(identityEnv, entry)
			continue
		}
		if name == "HOME" || name == "PATH" || name == "PWD" || name == "TMPDIR" || name == "XDG_CACHE_HOME" || name == "XDG_CONFIG_HOME" || name == "XDG_DATA_HOME" || name == "MESH_BACKEND" {
			args = append(args, "--env", entry)
		} else {
			args = append(args, "--env", name)
		}
	}
	clientEnv, err := containerEngineEnvironment(environment)
	if err != nil {
		return nil, setup, err
	}
	var identityFile *os.File
	if len(identityEnv) > 0 {
		identityFile, err = os.CreateTemp(jobPath, ".identity-env-*")
		if err != nil {
			return nil, setup, errors.New("could not prepare private container identity")
		}
		if _, err := identityFile.WriteString(strings.Join(identityEnv, "\n") + "\n"); err != nil {
			identityFile.Close()
			os.Remove(identityFile.Name())
			return nil, setup, errors.New("could not write private container identity")
		}
		if err := os.Remove(identityFile.Name()); err != nil {
			identityFile.Close()
			return nil, setup, errors.New("could not unlink private container identity")
		}
		if _, err := identityFile.Seek(0, 0); err != nil {
			identityFile.Close()
			return nil, setup, errors.New("could not rewind private container identity")
		}
		// The identity file has no name on disk. Podman reads the inherited fd,
		// while its own process keeps the worker account's USER and LOGNAME.
		args = append(args, "--env-file", "/proc/self/fd/3")
	}
	args = append(args, image.ID)
	args = append(args, containerArgs...)
	cmd := exec.Command(image.Runtime, args...)
	cmd.Env = clientEnv
	if identityFile != nil {
		cmd.ExtraFiles = []*os.File{identityFile}
	}
	if err := writeAtomic(filepath.Join(jobPath, "container.runtime"), []byte(image.Runtime+"\n")); err != nil {
		if identityFile != nil {
			identityFile.Close()
		}
		return nil, setup, errors.New("could not record job container runtime")
	}
	return cmd, setup, nil
}

func stopJobContainer(path, project, job string) {
	engine, err := privateRead(filepath.Join(path, "container.runtime"), 16)
	if err != nil || string(engine) != "docker\n" && string(engine) != "podman\n" {
		return
	}
	runtime := strings.TrimSpace(string(engine))
	content, err := os.ReadFile(filepath.Join(path, "container.cid"))
	if err != nil || len(content) > 128 {
		return
	}
	id := strings.TrimSpace(string(content))
	if !validImageID("sha256:" + id) {
		return
	}
	labels := exec.Command(runtime, "inspect", "--format", "{{index .Config.Labels \"mesh.job\"}} {{index .Config.Labels \"mesh.project\"}}", id)
	output, err := labels.Output()
	if err != nil || strings.TrimSpace(string(output)) != job+" "+project {
		return
	}
	cmd := exec.Command(runtime, "stop", "--time", strconv.Itoa(3), id)
	_ = cmd.Run()
}
