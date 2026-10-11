package switchyard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

var containerImagePattern = regexp.MustCompile(`^[a-zA-Z0-9._:/-]+@sha256:[a-f0-9]{64}$`)

func containerName(cwd string) string {
	identity := sha256.Sum256([]byte(strconv.Itoa(os.Getuid()) + "\x00" + cwd))
	return "switchyard-cwd-" + hex.EncodeToString(identity[:12])
}

type containerProfile struct {
	cwd      string
	image    string
	imageID  string
	name     string
	runtime  string
	network  string
	platform string
	user     string
}

func containerCWD(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("could not locate sandbox directory")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("sandbox directory is unavailable")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() || canonical == "/" || strings.Contains(canonical, ",") {
		return "", errors.New("sandbox needs a directory other than the filesystem root, without commas")
	}
	sockets := []string{"/var/run/docker.sock", "/run/docker.sock", "/run/podman/podman.sock",
		filepath.Join("/run/user", strconv.Itoa(os.Getuid()), "podman/podman.sock")}
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		sockets = append(sockets, strings.TrimPrefix(host, "unix://"))
	}
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		sockets = append(sockets, filepath.Join(runtimeDir, "docker.sock"), filepath.Join(runtimeDir, "podman/podman.sock"))
	}
	if userHome, err := os.UserHomeDir(); err == nil {
		sockets = append(sockets, filepath.Join(userHome, ".docker", "run", "docker.sock"))
	}
	for _, socket := range sockets {
		resolved, err := filepath.EvalSymlinks(socket)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(canonical, resolved)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return "", errors.New("sandbox CWD would expose a container control socket")
		}
	}
	return canonical, nil
}

func containerOutput(engine string, args ...string) (string, error) {
	cmd := exec.Command(engine, args...)
	output, err := cmd.Output()
	if err != nil {
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			switch {
			case strings.Contains(string(exited.Stderr), "bind source path does not exist"):
				return "", errors.New("container runtime cannot see the sandbox source directory")
			case strings.Contains(string(exited.Stderr), "invalid mount config"):
				return "", errors.New("container runtime rejected the sandbox mount")
			case strings.Contains(string(exited.Stderr), "permission denied"):
				return "", errors.New("container runtime denied sandbox access")
			case strings.Contains(string(exited.Stderr), "No such image"):
				return "", errors.New("container runtime could not find the pinned sandbox image")
			}
		}
		return "", errors.New("container runtime operation failed")
	}
	if len(output) > 16<<10 {
		return "", errors.New("Docker response is too large")
	}
	return strings.TrimSpace(string(output)), nil
}

func containerSettings(path string) (containerProfile, error) {
	cwd, err := containerCWD(path)
	if err != nil {
		return containerProfile{}, err
	}
	image := os.Getenv("SWITCHYARD_CONTAINER_IMAGE")
	if !containerImagePattern.MatchString(image) && !validImageID(image) {
		return containerProfile{}, errors.New("SWITCHYARD_CONTAINER_IMAGE must pin an image digest or full image ID")
	}
	network := os.Getenv("SWITCHYARD_CONTAINER_NETWORK")
	if network == "" {
		network = "none"
	}
	if network != "none" && network != "bridge" {
		return containerProfile{}, errors.New("SWITCHYARD_CONTAINER_NETWORK must be none or bridge")
	}
	platform := os.Getenv("SWITCHYARD_CONTAINER_PLATFORM")
	if platform == "" {
		platform = "linux/" + runtime.GOARCH
	}
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return containerProfile{}, errors.New("unsupported sandbox platform")
	}
	engine, err := containerRuntime()
	if err != nil {
		return containerProfile{}, err
	}
	imageID, err := containerOutput(engine, "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil || !snapshotPattern.MatchString(strings.TrimPrefix(imageID, "sha256:")) {
		if engine == "podman" && exec.Command("podman", "info").Run() != nil {
			return containerProfile{}, errors.New("rootless Podman is not ready; check user namespaces and subordinate UID/GID ranges")
		}
		return containerProfile{}, errors.New("sandbox image is not present locally; pull the pinned digest first")
	}
	return containerProfile{
		cwd: cwd, image: image, imageID: imageID,
		name: containerName(cwd), runtime: engine,
		network: network, platform: platform,
		user: fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
	}, nil
}

func containerLabels(profile containerProfile) []string {
	return []string{
		"--label", "switchyard.cwd=" + profile.cwd,
		"--label", "switchyard.image=" + profile.image,
		"--label", "switchyard.owner=" + strconv.Itoa(os.Getuid()),
		"--label", "switchyard.network=" + profile.network,
		"--label", "switchyard.platform=" + profile.platform,
		"--label", "switchyard.runtime=" + profile.runtime,
	}
}

func containerBase(profile containerProfile) []string {
	network := profile.network
	if profile.runtime == "podman" && network == "bridge" {
		network = "pasta"
	}
	options := []string{"--pull", "never", "--platform", profile.platform,
		"--network", network, "--user", profile.user,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--pids-limit", "512",
		"--mount", "type=bind,source=" + profile.cwd + ",target=" + profile.cwd,
		"--workdir", profile.cwd}
	if profile.runtime == "podman" {
		options = append(options, "--userns=keep-id", "--http-proxy=false")
	}
	return append(options, containerLabels(profile)...)
}

func inspectContainer(engine, name string) (map[string]string, bool, string, bool, error) {
	labels, err := containerOutput(engine, "container", "inspect", "--format", "{{json .Config.Labels}}", name)
	if err != nil {
		found, listErr := containerOutput(engine, "container", "ls", "-a", "--filter", "name=^/"+name+"$", "--format", "{{.Names}}")
		if listErr != nil {
			return nil, false, "", false, errors.New("could not inspect sandbox container")
		}
		if found == "" {
			return nil, false, "", false, nil
		}
		return nil, false, "", false, errors.New("could not inspect existing sandbox container")
	}
	var parsed map[string]string
	if json.Unmarshal([]byte(labels), &parsed) != nil {
		return nil, false, "", false, errors.New("invalid sandbox container labels")
	}
	imageID, err := containerOutput(engine, "container", "inspect", "--format", "{{.Image}}", name)
	if err != nil {
		return nil, false, "", false, err
	}
	running, err := containerOutput(engine, "container", "inspect", "--format", "{{.State.Running}}", name)
	if err != nil || running != "true" && running != "false" {
		return nil, false, "", false, errors.New("could not read sandbox container state")
	}
	return parsed, running == "true", imageID, true, nil
}

func verifyContainer(profile containerProfile, labels map[string]string, imageID string) error {
	if labels["switchyard.cwd"] != profile.cwd || labels["switchyard.image"] != profile.image ||
		labels["switchyard.owner"] != strconv.Itoa(os.Getuid()) || labels["switchyard.network"] != profile.network ||
		labels["switchyard.platform"] != profile.platform || imageID != profile.imageID ||
		labels["switchyard.runtime"] != profile.runtime && !(profile.runtime == "docker" && labels["switchyard.runtime"] == "") {
		return errors.New("CWD sandbox exists with a different image or settings; stop it explicitly before changing profiles")
	}
	return nil
}

func withContainerLock(home, name string, action func() error) error {
	directory := filepath.Join(home, "containers", name)
	if err := privateTree(directory); err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(directory, ".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return errors.New("could not lock CWD sandbox")
	}
	file := os.NewFile(uintptr(fd), ".lock")
	defer file.Close()
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return errors.New("could not lock CWD sandbox")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return action()
}

func sharedContainer(profile containerProfile, home string) error {
	return withContainerLock(home, profile.name, func() error {
		return startSharedContainer(profile, home)
	})
}

func startSharedContainer(profile containerProfile, home string) error {
	containerHome := filepath.Join(home, "containers", profile.name, "home")
	if err := privateTree(containerHome); err != nil {
		return err
	}
	engine, err := sharedContainerRuntime(home, profile.name)
	if err != nil {
		return err
	}
	if engine != "" && engine != profile.runtime {
		return errors.New("CWD sandbox uses another container runtime; stop it before changing profiles")
	}
	if engine == "" && profile.runtime != "docker" {
		_, _, _, legacy, legacyErr := inspectContainer("docker", profile.name)
		if legacyErr == nil && legacy {
			return errors.New("CWD sandbox uses legacy Docker; stop it before changing runtimes")
		}
	}
	labels, running, imageID, exists, err := inspectContainer(profile.runtime, profile.name)
	if err != nil {
		return err
	}
	if !exists {
		options := append([]string{"create", "--name", profile.name, "--init"}, containerBase(profile)...)
		options = append(options, "--mount", "type=bind,source="+containerHome+",target=/home/switchyard",
			profile.image, "/bin/sh", "-c", "exec tail -f /dev/null")
		if _, createErr := containerOutput(profile.runtime, options...); createErr != nil {
			// Another invocation may have created the same CWD container.
			labels, running, imageID, exists, err = inspectContainer(profile.runtime, profile.name)
			if err != nil || !exists {
				return errors.New("could not create sandbox container: " + createErr.Error())
			}
		} else {
			labels, running, imageID, exists, err = inspectContainer(profile.runtime, profile.name)
			if err != nil || !exists {
				return errors.New("could not verify new sandbox container")
			}
		}
	}
	if err := verifyContainer(profile, labels, imageID); err != nil {
		return err
	}
	if engine == "" {
		if err := writeAtomic(filepath.Join(home, "containers", profile.name, "runtime"), []byte(profile.runtime+"\n")); err != nil {
			return errors.New("could not record shared sandbox runtime")
		}
	}
	if !running {
		if _, err := containerOutput(profile.runtime, "start", profile.name); err != nil {
			labels, running, imageID, exists, _ = inspectContainer(profile.runtime, profile.name)
			if !exists || !running || verifyContainer(profile, labels, imageID) != nil {
				return errors.New("could not start sandbox container")
			}
		}
	}
	return nil
}

func sharedContainerRuntime(home, name string) (string, error) {
	path := filepath.Join(home, "containers", name, "runtime")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", errors.New("could not inspect shared sandbox runtime")
	}
	content, err := privateRead(path, 32)
	if err != nil || string(content) != "docker\n" && string(content) != "podman\n" {
		return "", errors.New("shared sandbox runtime record is invalid")
	}
	return strings.TrimSpace(string(content)), nil
}

func containerEnvironment(profile containerProfile) ([]string, []string, error) {
	environment := append([]string{}, os.Environ()...)
	environment = append(environment, "HOME=/home/switchyard", "PWD="+profile.cwd,
		"SWITCHYARD_CONTAINER_SESSION="+profile.name)
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" || strings.ContainsAny(name, "\x00\n\r") || strings.ContainsRune(value, 0) {
			return nil, nil, errors.New("invalid sandbox invocation environment")
		}
		if !containerEngineControl(name) && name != "XDG_RUNTIME_DIR" {
			values[name] = value
		}
	}
	values["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	values["TMPDIR"] = "/tmp"
	values["XDG_CACHE_HOME"] = "/home/switchyard/.cache"
	values["XDG_CONFIG_HOME"] = "/home/switchyard/.config"
	values["XDG_DATA_HOME"] = "/home/switchyard/.local/share"
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	forwarded := make([]string, 0, len(names))
	for _, name := range names {
		forwarded = append(forwarded, name+"="+values[name])
	}
	return names, forwarded, nil
}

func runContainerShell(home string, args []string) int {
	mode := os.Getenv("SWITCHYARD_CONTAINER_MODE")
	if mode == "" {
		mode = "shared"
	}
	if mode != "shared" && mode != "isolated" {
		return fail(errors.New("SWITCHYARD_CONTAINER_MODE must be shared or isolated"))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(errors.New("could not locate sandbox directory"))
	}
	profile, err := containerSettings(cwd)
	if err != nil {
		return fail(err)
	}
	names, environment, err := containerEnvironment(profile)
	if err != nil {
		return fail(err)
	}
	var runtimeArgs []string
	if mode == "shared" {
		if err := sharedContainer(profile, home); err != nil {
			return fail(err)
		}
		runtimeArgs = []string{"exec", "--interactive", "--workdir", profile.cwd}
	} else {
		runtimeArgs = append([]string{"run", "--rm", "--init", "--interactive"}, containerBase(profile)...)
		runtimeArgs = append(runtimeArgs, "--tmpfs", "/home/switchyard:rw,nosuid,size=256m")
	}
	if isTTY(os.Stdin) && isTTY(os.Stdout) {
		runtimeArgs = append(runtimeArgs, "--tty")
	}
	for _, name := range names {
		if name == "HOME" || name == "PWD" || name == "PATH" || name == "TMPDIR" ||
			name == "XDG_CACHE_HOME" || name == "XDG_CONFIG_HOME" || name == "XDG_DATA_HOME" {
			for _, entry := range environment {
				if strings.HasPrefix(entry, name+"=") {
					runtimeArgs = append(runtimeArgs, "--env", entry)
					break
				}
			}
		} else {
			runtimeArgs = append(runtimeArgs, "--env", name)
		}
	}
	if mode == "shared" {
		runtimeArgs = append(runtimeArgs, profile.name)
	} else {
		runtimeArgs = append(runtimeArgs, profile.image)
	}
	runtimeArgs = append(runtimeArgs, "/bin/bash")
	runtimeArgs = append(runtimeArgs, args...)
	binary, err := exec.LookPath(profile.runtime)
	if err != nil {
		return fail(errors.New("could not locate sandbox runtime"))
	}
	clientEnvironment, err := containerEngineEnvironment(environment)
	if err != nil {
		return fail(err)
	}
	if err := syscall.Exec(binary, append([]string{profile.runtime}, runtimeArgs...), clientEnvironment); err != nil {
		return fail(errors.New("could not attach to sandbox container"))
	}
	return 0
}

func containerControl(home, action, cwd string) int {
	canonical, err := containerCWD(cwd)
	if err != nil {
		return fail(err)
	}
	name := containerName(canonical)
	err = withContainerLock(home, name, func() error {
		return controlSharedContainer(home, action, canonical, name)
	})
	if err != nil {
		return fail(err)
	}
	return 0
}

func controlSharedContainer(home, action, canonical, name string) error {
	engine, err := sharedContainerRuntime(home, name)
	if err != nil {
		return err
	}
	if engine == "" {
		// Pre-runtime records were created exclusively by the Docker prototype.
		if _, lookupErr := exec.LookPath("docker"); lookupErr == nil {
			_, _, _, legacy, legacyErr := inspectContainer("docker", name)
			if legacyErr != nil {
				return legacyErr
			}
			if legacy {
				engine = "docker"
			}
		}
		if engine == "" {
			engine, err = containerRuntime()
			if err != nil {
				return err
			}
		}
	}
	labels, running, _, exists, err := inspectContainer(engine, name)
	if err != nil {
		return err
	}
	if !exists {
		if action == "stop" {
			if err := os.Remove(filepath.Join(home, "containers", name, "runtime")); err != nil && !os.IsNotExist(err) {
				return errors.New("could not clear stopped sandbox runtime")
			}
		}
		fmt.Println("No shared CWD sandbox")
		return nil
	}
	if labels["switchyard.cwd"] != canonical || labels["switchyard.owner"] != strconv.Itoa(os.Getuid()) {
		return errors.New("CWD sandbox identity does not match its container")
	}
	if action == "status" {
		if running {
			fmt.Println("Shared CWD sandbox: running")
		} else {
			fmt.Println("Shared CWD sandbox: stopped")
		}
		return nil
	}
	if action != "stop" {
		return errors.New("use container status or container stop")
	}
	if _, err := containerOutput(engine, "rm", "--force", name); err != nil {
		return errors.New("could not stop shared CWD sandbox")
	}
	if err := os.Remove(filepath.Join(home, "containers", name, "runtime")); err != nil && !os.IsNotExist(err) {
		return errors.New("could not clear stopped sandbox runtime")
	}
	fmt.Println("Shared CWD sandbox removed; private home kept in " + filepath.Join(home, "containers", name, "home"))
	return nil
}
