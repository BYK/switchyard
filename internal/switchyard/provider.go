package switchyard

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Provider is the execution boundary. Adapters own their endpoint and transfer
// method, while snapshotting, placement and the job contract remain shared.
type Provider interface {
	Name() string
	Ready(context.Context) bool
	Prepare(context.Context, string, string) error
	Sync(context.Context, string, string, string) error
	Call(context.Context, []string, bool) (int, []byte, error)
}

type backendConfig struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Root          string   `json:"root"`
	Host          string   `json:"host,omitempty"`
	User          string   `json:"user,omitempty"`
	Port          int      `json:"port,omitempty"`
	Identity      string   `json:"identity,omitempty"`
	KnownHosts    string   `json:"known_hosts,omitempty"`
	HostKeyAlias  string   `json:"host_key_alias,omitempty"`
	ObservationID string   `json:"observation_id,omitempty"`
	Command       string   `json:"command,omitempty"`
	Priority      int      `json:"priority,omitempty"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

type backendFile struct {
	Backends []backendConfig `json:"backends"`
}

func validateBackend(cfg backendConfig) error {
	if cfg.ObservationID != "" {
		if len(cfg.ObservationID) != 31 || cfg.ObservationID[0] != 'p' {
			return errors.New("invalid managed observation identity")
		}
		if _, err := hex.DecodeString(cfg.ObservationID[1:]); err != nil {
			return errors.New("invalid managed observation identity")
		}
	}
	if !remoteName.MatchString(cfg.Name) || strings.Contains(cfg.Name, ".") {
		return errors.New("backend name must use letters, numbers or hyphens")
	}
	if cfg.Name == "auto" || cfg.Name == "local" && cfg.Kind != "local" {
		return errors.New("auto and local are reserved backend names")
	}
	if !filepath.IsAbs(cfg.Root) || strings.Contains(cfg.Root, "..") {
		return errors.New("backend root must be an absolute path without '..'")
	}
	if cfg.Priority < 0 || cfg.Priority > 1000 {
		return errors.New("backend priority must be between 0 and 1000")
	}
	for _, capability := range cfg.Capabilities {
		if !remoteName.MatchString(capability) {
			return errors.New("invalid backend capability")
		}
	}
	switch cfg.Kind {
	case "local":
		if cfg.Host != "" || cfg.User != "" || cfg.Port != 0 || cfg.Command != "" || cfg.Identity != "" {
			return errors.New("local backend cannot set SSH fields")
		}
	case "plugin":
		if cfg.Host != "" || cfg.User != "" || cfg.Port != 0 || cfg.Identity != "" || cfg.KnownHosts != "" || cfg.HostKeyAlias != "" || !filepath.IsAbs(cfg.Command) {
			return errors.New("plugin needs an absolute executable and no SSH fields")
		}
		info, err := os.Lstat(cfg.Command)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return errors.New("plugin executable does not exist")
		}
	case "ssh":
		if !remoteName.MatchString(cfg.Host) || !remoteUser.MatchString(cfg.User) || !remoteName.MatchString(cfg.HostKeyAlias) || !remotePath.MatchString(cfg.Root) || !remotePath.MatchString(cfg.Command) || cfg.Port < 0 || cfg.Port > 65535 {
			return errors.New("invalid SSH backend endpoint")
		}
		for _, path := range []string{cfg.Identity, cfg.KnownHosts} {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || !filepath.IsAbs(path) {
				return errors.New("SSH identity and pinned known-hosts files must exist")
			}
		}
		cmd := exec.Command("ssh-keygen", "-F", cfg.HostKeyAlias, "-f", cfg.KnownHosts)
		if err := cmd.Run(); err != nil {
			return errors.New("the configured SSH alias is not pinned in known_hosts")
		}
	default:
		return errors.New("unsupported backend kind")
	}
	return nil
}

func configFile(home string) string {
	return filepath.Join(home, "backends.json")
}

func loadBackends(home string) ([]backendConfig, error) {
	path := configFile(home)
	content, err := privateRead(path, 1<<20)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		// The file is optional, but an invalid existing file is never ignored.
		if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
			return nil, nil
		}
		return nil, errors.New("backend configuration is invalid")
	}
	var data backendFile
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, errors.New("backend configuration is invalid")
	}
	seen := map[string]bool{}
	for _, cfg := range data.Backends {
		if err := validateBackend(cfg); err != nil || seen[cfg.Name] {
			return nil, errors.New("backend configuration has a duplicate or invalid entry")
		}
		seen[cfg.Name] = true
	}
	return data.Backends, nil
}

// A destroyed managed worker has no SSH key or known-hosts file. Drop only the
// matching managed backend without validating the now-absent key material.
func removeDestroyedAWSBackend(home, dir string) error {
	path := configFile(home)
	content, err := privateRead(path, 1<<20)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("backend configuration is invalid")
	}
	var data backendFile
	if err := json.Unmarshal(content, &data); err != nil {
		return errors.New("backend configuration is invalid")
	}
	remaining := make([]backendConfig, 0, len(data.Backends))
	removed := false
	name := awsBackendName(dir)
	for _, cfg := range data.Backends {
		if cfg.Name != name {
			remaining = append(remaining, cfg)
			continue
		}
		if removed || cfg.Kind != "ssh" || cfg.Root != awsRoot || cfg.Command != awsCommand ||
			cfg.Identity != awsKeyPath(dir) || cfg.KnownHosts != awsKnownPath(dir) || cfg.HostKeyAlias != awsName {
			return errors.New("managed AWS backend does not match the tracked worker")
		}
		removed = true
	}
	if !removed {
		return nil
	}
	content, err = json.MarshalIndent(backendFile{Backends: remaining}, "", "  ")
	if err != nil {
		return errors.New("could not update backend configuration")
	}
	return writeAtomic(path, append(content, '\n'))
}

func saveBackend(home string, cfg backendConfig) error {
	if err := validateBackend(cfg); err != nil {
		return err
	}
	entries, err := loadBackends(home)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name == cfg.Name {
			return errors.New("backend name is already configured")
		}
	}
	entries = append(entries, cfg)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Priority == entries[j].Priority {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Priority < entries[j].Priority
	})
	content, err := json.MarshalIndent(backendFile{Backends: entries}, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(configFile(home), append(content, '\n'))
}

func upsertAWSBackend(home string, cfg backendConfig) error {
	if cfg.Name != "aws" && cfg.Name != "aws-2" && cfg.Name != "aws-3" && cfg.Name != "aws-4" {
		return errors.New("invalid managed backend name")
	}
	if err := validateBackend(cfg); err != nil {
		return err
	}
	entries, err := loadBackends(home)
	if err != nil {
		return err
	}
	replaced := false
	for index, entry := range entries {
		if entry.Name != cfg.Name {
			continue
		}
		if entry.Kind != "ssh" || entry.HostKeyAlias != cfg.HostKeyAlias || entry.Identity != cfg.Identity || entry.KnownHosts != cfg.KnownHosts {
			return errors.New("aws backend name belongs to an unrelated worker")
		}
		// The same pinned instance can get a new public address after a stop.
		// Keep legacy measurements bound to that instance across the change.
		if entry.ObservationID == "" {
			cfg.ObservationID = observedBackendID(shellProvider{cfg: entry})
		} else {
			cfg.ObservationID = entry.ObservationID
		}
		entries[index], replaced = cfg, true
	}
	if !replaced {
		entries = append(entries, cfg)
	}
	content, err := json.MarshalIndent(backendFile{Backends: entries}, "", "  ")
	if err != nil {
		return errors.New("could not update managed worker configuration")
	}
	return writeAtomic(configFile(home), append(content, '\n'))
}

type shellProvider struct {
	cfg   backendConfig
	self  string
	quiet bool
}

type responseBuffer struct {
	data     bytes.Buffer
	limit    int
	tooLarge bool
}

func (buffer *responseBuffer) Write(content []byte) (int, error) {
	if buffer.data.Len()+len(content) > buffer.limit {
		buffer.tooLarge = true
		return 0, errors.New("worker response exceeds its size limit")
	}
	return buffer.data.Write(content)
}

func (provider shellProvider) Name() string { return provider.cfg.Name }

func (provider shellProvider) workerArgs(args []string) []string {
	return append([]string{"worker", "--root", provider.cfg.Root}, args...)
}

func (provider shellProvider) sshOptions() []string {
	options := []string{
		"-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes",
		"-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes",
		"-o", "ConnectTimeout=6", "-o", "IdentitiesOnly=yes",
		"-o", "UserKnownHostsFile=" + provider.cfg.KnownHosts,
		"-o", "HostKeyAlias=" + provider.cfg.HostKeyAlias,
		"-i", provider.cfg.Identity,
	}
	if provider.cfg.Port != 0 {
		options = append(options, "-p", strconv.Itoa(provider.cfg.Port))
	}
	if provider.quiet {
		options = append(options, "-o", "LogLevel=QUIET")
	}
	return options
}

func (provider shellProvider) command(ctx context.Context, request wireRequest) (*exec.Cmd, error) {
	args := request.Args
	wired := provider.cfg.Kind == "ssh" || request.Environment != nil || request.Quiet
	if wired {
		args = []string{"wire"}
	}
	params := provider.workerArgs(args)
	var cmd *exec.Cmd
	switch provider.cfg.Kind {
	case "local":
		cmd = exec.CommandContext(ctx, provider.self, params...)
	case "plugin":
		cmd = exec.CommandContext(ctx, provider.cfg.Command, params...)
	case "ssh":
		options := provider.sshOptions()
		// Only validated paths enter the remote login shell (including xonsh).
		options = append(options, provider.cfg.User+"@"+provider.cfg.Host,
			provider.cfg.Command+" worker --root "+provider.cfg.Root+" wire")
		cmd = exec.CommandContext(ctx, "ssh", options...)
	}
	if wired {
		content, err := json.Marshal(request)
		if err != nil || len(content)+1 > maxWireRequest {
			return nil, errors.New("worker request exceeds its size limit")
		}
		cmd.Stdin = bytes.NewReader(append(content, '\n'))
	}
	return cmd, nil
}

func (provider shellProvider) Call(ctx context.Context, args []string, stream bool) (int, []byte, error) {
	return provider.call(ctx, wireRequest{Args: args, Quiet: provider.quiet}, stream)
}

func (provider shellProvider) Execute(ctx context.Context, args, environment []string) (int, []byte, error) {
	if err := validateEnvironment(environment); err != nil {
		return 0, nil, err
	}
	encoded := make([][]byte, len(environment))
	for i, entry := range environment {
		encoded[i] = []byte(entry)
	}
	return provider.call(ctx, wireRequest{Args: args, Environment: &encoded, Quiet: provider.quiet}, true)
}

func (provider shellProvider) call(ctx context.Context, request wireRequest, stream bool) (int, []byte, error) {
	cmd, err := provider.command(ctx, request)
	if err != nil {
		return 0, nil, err
	}
	if stream {
		input, output, pipeErr := os.Pipe()
		if pipeErr != nil {
			return 0, nil, errors.New("could not prepare worker input")
		}
		original := cmd.Stdin
		cmd.Stdin = input
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err = cmd.Start(); err != nil {
			input.Close()
			output.Close()
			return 0, nil, errors.New("worker connection failed")
		}
		input.Close()
		go func() {
			defer output.Close()
			if original != nil {
				_, _ = io.Copy(output, original)
			}
			_, _ = io.Copy(output, os.Stdin)
		}()
		err = cmd.Wait()
		output.Close()
		if err == nil {
			return 0, nil, nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil, nil
		}
		return 0, nil, errors.New("worker connection failed")
	}
	limit := 1 << 20
	if len(request.Args) > 0 && request.Args[0] == "patch" {
		limit = maxPatchSize
	}
	buffer := &responseBuffer{limit: limit}
	cmd.Stdout = buffer
	err = cmd.Run()
	if buffer.tooLarge {
		return 0, nil, errors.New("worker response exceeds its size limit")
	}
	if err != nil {
		return 0, nil, errors.New("worker request failed")
	}
	return 0, buffer.data.Bytes(), nil
}

func (provider shellProvider) Ready(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	_, _, err := provider.Call(ctx, []string{"ready"}, false)
	return err == nil
}

func (provider shellProvider) Prepare(ctx context.Context, project, job string) error {
	_, _, err := provider.Call(ctx, []string{"prepare", "--project", project, "--job", job}, false)
	return err
}

type snapshotTransferError struct {
	message, reason string
	exitCode        int
}

func (failure *snapshotTransferError) Error() string { return failure.message }

func (provider shellProvider) Sync(ctx context.Context, tree, project, job string) error {
	if err := requireID(project); err != nil {
		return err
	}
	if err := requireID(job); err != nil {
		return err
	}
	if provider.cfg.Kind == "plugin" {
		cmd := exec.CommandContext(ctx, provider.cfg.Command, "sync", "--root", provider.cfg.Root,
			"--project", project, "--job", job, "--source", tree)
		if err := cmd.Run(); err != nil {
			return &snapshotTransferError{"plugin snapshot transfer failed", "adapter-failed", exitStatus(err)}
		}
		return nil
	}
	dest := filepath.Join(provider.cfg.Root, "projects", project, "jobs", job, "incoming", "files")
	args := []string{"-a", "--checksum", "--delete", "--no-whole-file", "--", tree + "/"}
	if provider.cfg.Kind == "ssh" {
		parts := append([]string{"ssh"}, provider.sshOptions()...)
		quoted := make([]string, 0, len(parts))
		for _, part := range parts {
			quoted = append(quoted, "'"+strings.ReplaceAll(part, "'", "'\\''")+"'")
		}
		args = append([]string{"-e", strings.Join(quoted, " "), "--rsync-path=/usr/bin/rsync"}, args...)
		dest = provider.cfg.User + "@" + provider.cfg.Host + ":" + dest
	}
	args = append(args, dest+"/")
	cmd := exec.CommandContext(ctx, "rsync", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		failure := ""
		reason := "rsync-failed"
		for _, candidate := range []struct{ token, label, result string }{
			{"No space left on device", "worker disk is full", "disk-full"},
			{"Host key verification failed", "SSH host key verification failed", "host-key"},
			{"Permission denied", "remote permission denied", "permission"},
			{"No such file or directory", "transfer path or remote rsync is missing", "missing-path"},
			{"Connection refused", "SSH connection refused", "connection-refused"},
			{"protocol version mismatch", "remote shell interfered with rsync", "protocol"},
			{"mkstemp", "remote temporary file could not be created", "temporary-file"},
		} {
			if strings.Contains(string(output), candidate.token) {
				failure = ": " + candidate.label
				reason = candidate.result
				break
			}
		}
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			return &snapshotTransferError{fmt.Sprintf("snapshot transfer to %s failed (rsync exit %d%s)", provider.Name(), exited.ExitCode(), failure), reason, exited.ExitCode()}
		}
		return &snapshotTransferError{fmt.Sprintf("snapshot transfer to %s failed", provider.Name()), reason, -1}
	}
	return nil
}

func providers(home, self string) ([]shellProvider, error) {
	configs, err := loadBackends(home)
	if err != nil {
		return nil, err
	}
	result := make([]shellProvider, 0, len(configs))
	for _, cfg := range configs {
		result = append(result, shellProvider{cfg: cfg, self: self})
	}
	return result, nil
}
