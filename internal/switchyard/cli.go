package switchyard

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type includesFlag []string

func (paths *includesFlag) String() string { return strings.Join(*paths, ",") }
func (paths *includesFlag) Set(path string) error {
	*paths = append(*paths, path)
	return nil
}

type localJob struct {
	Project  string `json:"project"`
	Snapshot string `json:"snapshot"`
	Backend  string `json:"backend"`
	Endpoint string `json:"endpoint"`
}

func pruneLocalRecords(home string) error {
	path := filepath.Join(home, "runs")
	if err := privateTree(path); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return errors.New("could not inspect old job records")
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || requireID(strings.TrimSuffix(entry.Name(), ".json")) != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || time.Since(info.ModTime()) < 7*24*time.Hour {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if err := os.Remove(filepath.Join(path, id+".patch")); err != nil && !os.IsNotExist(err) {
			return errors.New("could not reclaim old job patch")
		}
		if err := os.Remove(filepath.Join(path, entry.Name())); err != nil {
			return errors.New("could not reclaim old job record")
		}
	}
	return nil
}

func endpointID(cfg backendConfig) string {
	sum := sha256.Sum256([]byte(cfg.Kind + "\x00" + cfg.Root + "\x00" + cfg.Host + "\x00" + cfg.User + "\x00" + cfg.HostKeyAlias + "\x00" + strconv.Itoa(cfg.Port)))
	return hex.EncodeToString(sum[:])
}

func defaultLocal(home string) backendConfig {
	return backendConfig{Name: "local", Kind: "local", Root: filepath.Join(home, "worker"), Capabilities: []string{"cpu"}}
}

func currentBackend(home, self, name string) (shellProvider, error) {
	list, err := providers(home, self)
	if err != nil {
		return shellProvider{}, err
	}
	for _, provider := range list {
		if provider.Name() == name {
			return provider, nil
		}
	}
	if name == "local" {
		return shellProvider{cfg: defaultLocal(home), self: self}, nil
	}
	return shellProvider{}, errors.New("unknown backend")
}

func supports(cfg backendConfig, required []string) bool {
	available := cfg.Capabilities
	if len(available) == 0 {
		available = []string{"cpu"}
	}
	for _, capability := range required {
		found := false
		for _, candidate := range available {
			if capability == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func pressured() (bool, string) {
	cpu, _ := os.ReadFile("/proc/pressure/cpu")
	memory, _ := os.ReadFile("/proc/meminfo")
	return pressureFrom(cpu, memory)
}

func pressureFrom(cpu, memory []byte) (bool, string) {
	if len(cpu) > 0 {
		for _, field := range strings.Fields(strings.SplitN(string(cpu), "\n", 2)[0]) {
			if strings.HasPrefix(field, "avg10=") {
				value, err := strconv.ParseFloat(strings.TrimPrefix(field, "avg10="), 64)
				if err == nil && value >= 20 {
					return true, "CPU contention"
				}
			}
		}
	}
	if len(memory) > 0 {
		var total, free uint64
		for _, line := range strings.Split(string(memory), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				total, _ = strconv.ParseUint(fields[1], 10, 64)
			case "MemAvailable:":
				free, _ = strconv.ParseUint(fields[1], 10, 64)
			}
		}
		if total > 0 && free*5 < total {
			return true, "low available memory"
		}
	}
	return false, "local resources available"
}

func selectProvider(ctx context.Context, home, self, requested string, required []string) (shellProvider, string, error) {
	if requested != "auto" {
		provider, err := currentBackend(home, self, requested)
		if err != nil {
			if !isAWSWorker(requested) {
				return shellProvider{}, "", err
			}
			dir, dirErr := awsDirectory()
			if dirErr != nil {
				return shellProvider{}, "", dirErr
			}
			if waitErr := awsWaitReady(ctx, home, self, requested, dir); waitErr != nil {
				return shellProvider{}, "", waitErr
			}
			provider, err = currentBackend(home, self, requested)
			if err != nil {
				return shellProvider{}, "", err
			}
		}
		if !supports(provider.cfg, required) {
			return shellProvider{}, "", errors.New("backend does not advertise the required capabilities")
		}
		if provider.cfg.Kind == "local" {
			if err := initWorker(provider.cfg.Root); err != nil {
				return shellProvider{}, "", err
			}
		} else if !provider.Ready(ctx) {
			if !isAWSWorker(requested) {
				return shellProvider{}, "", errors.New("requested backend is not ready")
			}
			dir, err := awsDirectory()
			if err != nil {
				return shellProvider{}, "", err
			}
			if err := awsWaitReady(ctx, home, self, requested, dir); err != nil {
				return shellProvider{}, "", err
			}
			provider, err = currentBackend(home, self, requested)
			if err != nil {
				return shellProvider{}, "", err
			}
		}
		return provider, "explicit backend", nil
	}
	busy, reason := pressured()
	return selectProviderAtPressure(ctx, home, self, required, busy, reason)
}

func selectProviderAtPressure(ctx context.Context, home, self string, required []string, busy bool, reason string) (shellProvider, string, error) {
	local, err := currentBackend(home, self, "local")
	if err != nil {
		return shellProvider{}, "", err
	}
	if !supports(local.cfg, required) {
		reason = "required capability unavailable locally"
	}
	if busy || !supports(local.cfg, required) {
		list, err := providers(home, self)
		if err != nil {
			return shellProvider{}, "", err
		}
		for _, provider := range list {
			if provider.cfg.Kind != "local" && supports(provider.cfg, required) && provider.Ready(ctx) {
				if isAWSWorker(provider.Name()) {
					available, err := awsWorkerAvailable(provider.Name())
					if err != nil {
						return shellProvider{}, "", err
					}
					if !available {
						awsRequestScale()
						continue
					}
				}
				return provider, reason, nil
			}
		}
		if busy {
			awsRequestWake()
		}
		if !supports(local.cfg, required) {
			return shellProvider{}, "", errors.New("no ready backend supports the required capabilities")
		}
		reason += "; no remote backend is ready"
	}
	if err := initWorker(local.cfg.Root); err != nil {
		return shellProvider{}, "", err
	}
	return local, reason, nil
}

func printSelection(chosen selection, details bool) {
	fmt.Printf("Snapshot: %d files; %d private paths excluded\n", len(chosen.Paths), len(chosen.Excluded))
	if details {
		for _, path := range chosen.Paths {
			fmt.Printf("  + %s\n", path)
		}
		for _, path := range chosen.Excluded {
			fmt.Printf("  - %s (excluded)\n", path)
		}
	}
	if chosen.VCS == "jj" {
		fmt.Println("jj ignored or oversized new files require --include")
	}
}

func saveLocalJob(home, id string, record localJob) error {
	if err := requireID(id); err != nil {
		return err
	}
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "runs", id+".json")
	if _, err := os.Lstat(path); err == nil {
		return errors.New("job ID already exists")
	}
	return writeAtomic(path, append(content, '\n'))
}

func loadLocalJob(home, id string) (localJob, error) {
	if err := requireID(id); err != nil {
		return localJob{}, err
	}
	content, err := privateRead(filepath.Join(home, "runs", id+".json"), 4096)
	if err != nil {
		return localJob{}, errors.New("unknown local job")
	}
	var record localJob
	if json.Unmarshal(content, &record) != nil || requireID(record.Project) != nil || requireSnapshot(record.Snapshot) != nil || record.Backend == "" {
		return localJob{}, errors.New("invalid local job record")
	}
	return record, nil
}

func listJobs(home string) int {
	entries, err := os.ReadDir(filepath.Join(home, "runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		return fail(errors.New("could not list local job records"))
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || requireID(id) != nil {
			continue
		}
		record, err := loadLocalJob(home, id)
		if err != nil {
			return fail(err)
		}
		patch := ""
		if info, err := os.Lstat(filepath.Join(home, "runs", id+".patch")); err == nil && info.Mode().IsRegular() {
			patch = " patch=" + filepath.Join(home, "runs", id+".patch")
		}
		fmt.Printf("%s %s snapshot=%s%s\n", id, record.Backend, record.Snapshot, patch)
	}
	return 0
}

func parsedStatus(provider shellProvider, project, job string) (jobRecord, error) {
	_, response, err := provider.Call(context.Background(), []string{"status", "--project", project, "--job", job}, false)
	if err != nil {
		return jobRecord{}, err
	}
	var status jobRecord
	if json.Unmarshal(response, &status) != nil || status.ID != job || status.Project != project {
		return jobRecord{}, errors.New("worker returned an invalid job status")
	}
	return status, nil
}

func executeJob(home, self, start, backend string, includes, required []string, argv []string, details, proxy bool) int {
	if len(argv) == 0 {
		return fail(errors.New("supply a command after --"))
	}
	for _, capability := range required {
		if !remoteName.MatchString(capability) {
			return fail(errors.New("invalid required capability"))
		}
	}
	environment := os.Environ()
	if err := validateEnvironment(environment); err != nil {
		return fail(err)
	}
	started := time.Now()
	provider, reason, err := selectProvider(context.Background(), home, self, backend, required)
	if err != nil {
		return fail(err)
	}
	if isAWSWorker(provider.Name()) {
		dir, dirErr := awsDirectory()
		if dirErr != nil {
			return fail(dirErr)
		}
		lease, leaseErr := awsTryLease(dir, provider.Name())
		if leaseErr != nil {
			return fail(leaseErr)
		}
		if lease == nil {
			awsRequestScale()
			if backend != "auto" && !(os.Getenv("SWITCHYARD_TRACE_HANDOFF") == "1" && os.Getenv("SWITCHYARD_BACKEND") == "auto") {
				return fail(errors.New("AWS worker is occupied; retry after the active job completes"))
			}
			provider, leaseErr = currentBackend(home, self, "local")
			if leaseErr != nil {
				return fail(leaseErr)
			}
			if leaseErr = initWorker(provider.cfg.Root); leaseErr != nil {
				return fail(leaseErr)
			}
			reason = "AWS worker occupied; using local worker"
		} else {
			defer lease.Close()
			if err := awsNoteWorkerUse(dir, provider.Name()); err != nil {
				return fail(err)
			}
			// This runs before lease.Close, so idle reconciliation sees the new
			// idle timestamp only after this dispatch has fully completed.
			defer func() {
				if err := awsNoteWorkerUse(dir, provider.Name()); err != nil {
					fmt.Fprintln(os.Stderr, "switchyard: AWS worker activity could not be saved")
				}
			}()
		}
	}
	if proxy && provider.Name() == "local" {
		path, err := exec.LookPath(argv[0])
		if err != nil {
			return fail(errors.New("could not locate local command"))
		}
		if err := syscall.Exec(path, argv, os.Environ()); err != nil {
			return fail(errors.New("could not start local command"))
		}
		return 0
	}
	if err := pruneLocalRecords(home); err != nil {
		return fail(err)
	}
	chosen, err := selectFiles(start, includes)
	if err != nil {
		return fail(err)
	}
	if provider.cfg.Kind == "local" && (chosen.Root == provider.cfg.Root || strings.HasPrefix(provider.cfg.Root, chosen.Root+string(os.PathSeparator))) {
		return fail(errors.New("local worker storage must be outside the source worktree"))
	}
	if !proxy {
		printSelection(chosen, details)
		fmt.Printf("Backend: %s (%s)\n", provider.Name(), reason)
	}
	captured, current, err := captureFiles(start, includes, home)
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(filepath.Dir(captured.Tree))
	if !equalPaths(chosen.Paths, current.Paths) || !equalPaths(chosen.Excluded, current.Excluded) || chosen.Root != current.Root {
		return fail(errors.New("file selection changed during capture"))
	}
	if !proxy {
		fmt.Printf("Captured %s (%.2fs)\n", captured.Snapshot, time.Since(started).Seconds())
	}
	seed := sha256.Sum256([]byte(chosen.Root + "\x00" + provider.Name() + "\x00" + endpointID(provider.cfg)))
	project := hex.EncodeToString(seed[:16])
	job, err := randomID()
	if err != nil {
		return fail(err)
	}
	if err := provider.Prepare(context.Background(), project, job); err != nil {
		return fail(err)
	}
	if err := provider.Sync(context.Background(), captured.Tree, project, job); err != nil {
		return fail(err)
	}
	if err := saveLocalJob(home, job, localJob{Project: project, Snapshot: captured.Snapshot, Backend: provider.Name(), Endpoint: endpointID(provider.cfg)}); err != nil {
		return fail(err)
	}
	if !proxy {
		fmt.Printf("Job %s; worker command starts after snapshot transfer\n", job)
	}
	args := []string{"execute", "--project", project, "--job", job, "--snapshot", captured.Snapshot, "--cwd", chosen.CWD, "--"}
	args = append(args, argv...)
	ctx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(interrupts)
	done := make(chan struct{})
	defer close(done)
	var interrupted atomic.Int32
	go func() {
		select {
		case received := <-interrupts:
			interrupted.Store(int32(128 + received.(syscall.Signal)))
			cancelRun()
		case <-done:
		}
	}()
	exit, _, err := provider.Execute(ctx, args, environment)
	if ctx.Err() != nil {
		cancelCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_, _, cancelErr := provider.Call(cancelCtx, []string{"cancel", "--project", project, "--job", job}, false)
		if cancelErr != nil {
			fmt.Fprintln(os.Stderr, "switchyard: worker cancellation could not be confirmed; use switchyard cancel "+job)
		}
		return int(interrupted.Load())
	}
	if err != nil {
		return fail(err)
	}
	status, err := parsedStatus(provider, project, job)
	if err == nil && status.State == "setup-failed" && status.Snapshot == captured.Snapshot && status.SetupStatus != nil {
		if !proxy {
			fmt.Printf("Job %s: dependency setup failed (exit %d); requested command did not start\n", job, *status.SetupStatus)
		}
		if *status.SetupStatus >= 128 {
			return *status.SetupStatus
		}
		return 2
	}
	if err != nil || status.State != "finished" || status.ExitStatus == nil || status.Snapshot != captured.Snapshot || *status.ExitStatus != exit {
		return fail(errors.New("worker did not confirm the command exit status and snapshot"))
	}
	if status.HasEdits {
		_, patch, err := provider.Call(context.Background(), []string{"patch", "--project", project, "--job", job}, false)
		if err != nil || len(patch) > maxPatchSize {
			return fail(errors.New("could not retrieve worker edits"))
		}
		path := filepath.Join(home, "runs", job+".patch")
		if err := writeAtomic(path, patch); err != nil {
			return fail(err)
		}
		if !proxy {
			fmt.Printf("Worker edits: %s (review before applying)\n", path)
		}
	}
	if status.EditsError != "" {
		fmt.Fprintln(os.Stderr, "switchyard: "+status.EditsError)
	}
	if provider.cfg.Kind != "local" {
		executable, lookupErr := exec.LookPath(argv[0])
		if filepath.Base(argv[0]) == argv[0] {
			if original, err := realTool(argv[0], self); err == nil {
				executable, lookupErr = original, nil
			}
		}
		if lookupErr == nil {
			if absolute, pathErr := filepath.Abs(executable); pathErr == nil {
				if signature, ok := commandSignature(start, absolute, argv[1:]); ok {
					// A failed observation must never replace a completed job's exit status.
					if err := recordCommandObservation(home, signature, observedBackendID(provider), time.Since(started), 0); err != nil {
						fmt.Fprintln(os.Stderr, "switchyard: private command timing could not be saved")
					}
				}
			}
		}
	}
	if !proxy {
		fmt.Printf("Snapshot %s; job %s; exit %d; elapsed %.2fs\n", captured.Snapshot, job, exit, time.Since(started).Seconds())
	}
	return exit
}

func existingJob(home, self, action, job string) int {
	record, err := loadLocalJob(home, job)
	if err != nil {
		return fail(err)
	}
	provider, err := currentBackend(home, self, record.Backend)
	if err != nil || record.Endpoint != endpointID(provider.cfg) {
		return fail(errors.New("job belongs to another backend configuration"))
	}
	args := []string{action, "--project", record.Project, "--job", job}
	if action == "logs" {
		_, _, err := provider.Call(context.Background(), args, true)
		if err != nil {
			return fail(err)
		}
		return 0
	}
	_, response, err := provider.Call(context.Background(), args, false)
	if err != nil {
		return fail(err)
	}
	var state jobRecord
	if json.Unmarshal(response, &state) != nil || state.ID != job || state.Project != record.Project {
		return fail(errors.New("invalid worker response"))
	}
	exit := "unknown"
	if state.ExitStatus != nil {
		exit = strconv.Itoa(*state.ExitStatus)
	}
	fmt.Printf("Job %s: %s; snapshot %s; exit %s\n", job, state.State, state.Snapshot, exit)
	if state.EditsError != "" {
		fmt.Println("Worker edits: collection failed")
	} else if state.HasEdits {
		fmt.Println("Worker edits: available")
	} else if state.State == "finished" {
		fmt.Println("Worker edits: none")
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "switchyard: "+err.Error())
	return 2
}

func addBackend(home string, args []string) int {
	flags := flag.NewFlagSet("backend add", flag.ContinueOnError)
	name := flags.String("name", "", "unique backend name")
	kind := flags.String("kind", "local", "local, ssh or plugin")
	root := flags.String("root", "", "private worker directory")
	host := flags.String("host", "", "SSH host")
	user := flags.String("user", "", "SSH user")
	port := flags.Int("port", 0, "SSH port (default 22)")
	identity := flags.String("identity", "", "SSH identity file")
	knownHosts := flags.String("known-hosts", "", "pinned known_hosts file")
	alias := flags.String("host-key-alias", "", "verified SSH host identity")
	command := flags.String("worker-command", "", "absolute worker binary on SSH host or plugin executable")
	priority := flags.Int("priority", 100, "lower priority is selected first")
	var capabilities includesFlag
	flags.Var(&capabilities, "capability", "backend capability, repeatable (default cpu)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if len(flags.Args()) > 0 {
		return fail(errors.New("unexpected backend arguments"))
	}
	if len(capabilities) == 0 {
		capabilities = append(capabilities, "cpu")
	}
	cfg := backendConfig{Name: *name, Kind: *kind, Root: *root, Host: *host, User: *user, Port: *port,
		Identity: *identity, KnownHosts: *knownHosts, HostKeyAlias: *alias, Command: *command, Priority: *priority, Capabilities: capabilities}
	if err := saveBackend(home, cfg); err != nil {
		return fail(err)
	}
	fmt.Println("Configured backend: " + cfg.Name)
	return 0
}

func printHelp() {
	fmt.Println("switchyard: frozen worktree execution on local and operator-controlled workers")
	fmt.Println("  switchyard backend add --name NAME --kind local|ssh|plugin --root PATH [options]")
	fmt.Println("  switchyard backend list")
	fmt.Println("  switchyard preview [--repo PATH] [--include PATH]")
	fmt.Println("  switchyard run [--backend auto|NAME] [--repo PATH] [--include PATH] [--require CAPABILITY] -- COMMAND ARGS...")
	fmt.Println("  switchyard proxy [--backend auto|NAME] [--repo PATH] -- COMMAND ARGS...")
	fmt.Println("  switchyard jobs                    list local run IDs and unapplied patches")
	fmt.Println("  switchyard stats [--limit N]       recent command timing samples by backend")
	fmt.Println("  switchyard status|logs|cancel JOB_ID")
	fmt.Println("  switchyard hook [--] TOOL ARGS...   (managed PATH shims)")
	fmt.Println("  switchyard hook --shell --resolved PATH -- TOOL ARGS...   (patched Bash external command hook)")
	fmt.Println("  switchyard trace install           (private Linux x86-64 Bash launch link)")
	fmt.Println("  switchyard trace shell -- -c COMMAND   (opt-in exec-filtered Bash supervisor)")
	fmt.Println("  switchyard image build|status       (pinned Node image in the selected Linux runtime)")
	fmt.Println("  switchyard aws auth select PROFILE [--role ROLE] | auth status")
	fmt.Println("  switchyard aws up|status|setup|down|destroy [--source PATH]")
	fmt.Println("  switchyard aws auto enable --source ABSOLUTE_PATH [--max-workers 1..4]")
	fmt.Println("  switchyard aws auto status|tick|disable")
	fmt.Println("  switchyard container status|stop [--cwd PATH]   (opt-in CWD sandbox)")
	fmt.Println("  switchyard worker --root PATH init|ready|load|prepare|execute|status|logs|patch|cancel")
}

func Main(args []string) int {
	self, err := os.Executable()
	if err != nil {
		return fail(errors.New("could not locate switchyard binary"))
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fail(errors.New("could not resolve switchyard binary"))
	}
	home, err := dataHome()
	if err != nil {
		return fail(err)
	}
	if os.Getenv("SWITCHYARD_TRACE_HANDOFF") == "1" && filepath.IsAbs(os.Args[0]) {
		invoked, invokedErr := os.Stat(os.Args[0])
		executable, executableErr := os.Stat(self)
		if invokedErr != nil || executableErr != nil {
			return fail(errors.New("traced command executable is unavailable"))
		}
		if !os.SameFile(invoked, executable) {
			return runHook(home, self, append([]string{"--shell", "--resolved", os.Args[0], "--", filepath.Base(os.Args[0])}, args...))
		}
	}
	if tool := filepath.Base(os.Args[0]); hookTools[tool] {
		return runHook(home, self, append([]string{tool}, args...))
	}
	if filepath.Base(os.Args[0]) == "bash" && os.Getenv("SWITCHYARD_TRACE_SHELL") == "1" {
		return traceShell(self, args)
	}
	if filepath.Base(os.Args[0]) == "switchyard-shell" {
		return runShell(home, self, args)
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		printHelp()
		return 0
	}
	switch args[0] {
	case "stats":
		flags := flag.NewFlagSet("stats", flag.ContinueOnError)
		limit := flags.Int("limit", 10, "maximum number of recent command signatures (1-50)")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if len(flags.Args()) != 0 || *limit < 1 || *limit > 50 {
			return fail(errors.New("stats accepts only --limit 1..50"))
		}
		list, err := providers(home, self)
		if err != nil {
			return fail(err)
		}
		if err := writeStatistics(home, list, os.Stdout, *limit); err != nil {
			return fail(err)
		}
		return 0
	case "jobs":
		if len(args) != 1 {
			return fail(errors.New("jobs takes no arguments"))
		}
		return listJobs(home)
	case "backend":
		if len(args) < 2 {
			return fail(errors.New("use backend add or backend list"))
		}
		if args[1] == "add" {
			return addBackend(home, args[2:])
		}
		if args[1] != "list" || len(args) != 2 {
			return fail(errors.New("use backend add or backend list"))
		}
		list, err := providers(home, self)
		if err != nil {
			return fail(err)
		}
		fmt.Println("local\tlocal (built-in)")
		for _, provider := range list {
			fmt.Printf("%s\t%s\n", provider.Name(), provider.cfg.Kind)
		}
		return 0
	case "preview", "run", "proxy":
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		repo := flags.String("repo", ".", "source Git or jj worktree")
		backend := flags.String("backend", "auto", "auto, local or a configured backend")
		var includes includesFlag
		flags.Var(&includes, "include", "explicit ignored or oversized file")
		var required includesFlag
		flags.Var(&required, "require", "required backend capability, repeatable")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		if args[0] == "preview" {
			if len(flags.Args()) != 0 {
				return fail(errors.New("preview takes no command"))
			}
			chosen, err := selectFiles(*repo, includes)
			if err != nil {
				return fail(err)
			}
			printSelection(chosen, true)
			return 0
		}
		return executeJob(home, self, *repo, *backend, includes, required, flags.Args(), true, args[0] == "proxy")
	case "status", "logs", "cancel":
		if len(args) != 2 {
			return fail(errors.New("supply exactly one job ID"))
		}
		return existingJob(home, self, args[0], args[1])
	case "worker":
		flags := flag.NewFlagSet("worker", flag.ContinueOnError)
		root := flags.String("root", "", "private worker directory")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		rest := flags.Args()
		var environment []string
		var stdin io.Reader = os.Stdin
		if len(rest) == 1 && rest[0] == "wire" {
			reader := bufio.NewReader(os.Stdin)
			line := make([]byte, 0, 4096)
			for {
				chunk, readErr := reader.ReadSlice('\n')
				if len(line)+len(chunk) > maxWireRequest {
					return fail(errors.New("worker request exceeds its size limit"))
				}
				line = append(line, chunk...)
				if readErr == nil {
					break
				}
				if readErr != bufio.ErrBufferFull {
					return fail(errors.New("invalid worker request"))
				}
			}
			var request wireRequest
			if json.Unmarshal(line, &request) != nil || len(request.Args) == 0 {
				return fail(errors.New("invalid worker request"))
			}
			if request.Environment != nil {
				if request.Args[0] != "execute" {
					return fail(errors.New("invalid worker request"))
				}
				environment = make([]string, len(*request.Environment))
				for i, entry := range *request.Environment {
					environment[i] = string(entry)
				}
				if err := validateEnvironment(environment); err != nil {
					return fail(err)
				}
			}
			rest = request.Args
			stdin = reader
		}
		status, err := dispatchWorker(*root, rest, environment, stdin)
		if err != nil {
			return fail(err)
		}
		return status
	case "hook":
		return runHook(home, self, args[1:])
	case "trace":
		return traceCommand(home, self, args[1:])
	case "aws":
		return runAWS(home, args[1:])
	case "image":
		return runImage(home, args[1:])
	case "container":
		if len(args) < 2 || args[1] != "status" && args[1] != "stop" {
			return fail(errors.New("use container status or container stop"))
		}
		flags := flag.NewFlagSet("container "+args[1], flag.ContinueOnError)
		cwd := flags.String("cwd", ".", "sandbox directory")
		if err := flags.Parse(args[2:]); err != nil || len(flags.Args()) != 0 {
			return 2
		}
		return containerControl(home, args[1], *cwd)
	default:
		return fail(errors.New("unknown command; run switchyard help"))
	}
}

type wireRequest struct {
	Args        []string  `json:"args"`
	Environment *[][]byte `json:"environment,omitempty"`
}

func dispatchWorker(root string, rest []string, environment []string, stdin io.Reader) (int, error) {
	if len(rest) == 0 {
		return 2, errors.New("worker action required")
	}
	step := flag.NewFlagSet("worker "+rest[0], flag.ContinueOnError)
	step.SetOutput(io.Discard)
	project := step.String("project", "", "project ID")
	job := step.String("job", "", "job ID")
	snapshot := step.String("snapshot", "", "snapshot hash")
	cwd := step.String("cwd", ".", "directory within snapshot")
	if err := step.Parse(rest[1:]); err != nil {
		return 2, errors.New("invalid worker request")
	}
	return runWorker(root, rest[0], *project, *job, *snapshot, *cwd, step.Args(), environment, stdin)
}
