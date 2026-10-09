# Switchyard

Run a command on a prepared worker against one fixed snapshot of your local Git or Jujutsu worktree. Your project remote is never contacted. Each job gets a separate writable checkout, streams output, returns the command's exit status, and saves agent edits as a patch for review.

This is a single-user CLI. It can manage one AWS London worker; it does not provide hosted accounts. The worker runs under a dedicated OS account on a machine you control. Use a separate worker for each trust boundary; arbitrary commands can access that account's files and network.

## Install

Build the Go 1.22+ CLI on Linux. The client needs Git, rsync and OpenSSH; install `jj` locally for `jj` projects. A remote client does not need a container runtime. The worker needs Git, rsync, rootless Podman with `catatonit`, and the image for the commands it runs.

```sh
go build -trimpath -o "$HOME/.local/bin/switchyard" ./cmd/switchyard
switchyard help
```

### AWS London worker

On `labs`, AWS CLI login must already work. From this source checkout:

```sh
switchyard aws up --source /home/byk/Code/compute-mesh
switchyard aws status
# Once toolchain ready is true:
switchyard aws setup --source /home/byk/Code/compute-mesh
switchyard preview --repo /absolute/project/path
switchyard run --backend aws --repo /absolute/project/path -- pnpm test
switchyard aws down
switchyard aws status          # Confirm stopped
switchyard aws destroy         # Only when finished permanently; repeat after termination until verified
```

`switchyard aws up` creates or starts a tracked on-demand `m7i.xlarge` in AWS London on Ubuntu 24.04, with an encrypted 30-GB disk, IMDSv2, and SSH restricted to your current public IPv4 address. It compares the SSH host key to the EC2 console fingerprint, pins it, and refreshes the public IP after a restart. `aws setup` installs rootless Podman, checks the two-hour auto-stop timer, transfers the Go binary over pinned SSH, builds the pinned Node image on the worker, and registers the SSH backend. The timer can stop a long job. `aws down` stops compute billing; the disk remains billable until `aws destroy` completes. If startup is still in progress, use a delayed follow-up, not repeated dispatches. Never display account data, keys, or raw AWS error payloads.

For a manually managed worker, copy the same Go binary to that machine, initialize its private worker directory, and verify and pin its SSH host key before adding it:

```sh
switchyard worker --root /private/worker/path init
switchyard backend add --name worker1 --kind ssh --root /private/worker/path \
  --host worker.example.com --user ubuntu --identity /absolute/path/to/key \
  --known-hosts /absolute/path/to/pinned_known_hosts \
  --host-key-alias worker1 --worker-command /absolute/path/to/switchyard \
  --priority 10 --capability cpu
```

From a project directory, inspect **every selected path and exclusion** before every dispatch:

```sh
switchyard preview --repo /absolute/project/path
switchyard run --backend aws --repo /absolute/project/path -- pnpm test
switchyard status JOB_ID
switchyard logs JOB_ID
switchyard cancel JOB_ID
```

`switchyard run` prints the job ID, content-hashed snapshot, elapsed time and exact exit status. If the command edits its checkout, Switchyard saves a binary-capable Git patch under `~/.local/share/compute-mesh-go/runs/JOB_ID.patch`. Review it before applying it locally. Switchyard never applies worker edits to your live worktree. Worker logs are capped at 128 MiB and returned patches at 64 MiB; command output streams live.

`switchyard run` forwards the invoking process's exported environment through a bounded stdin request, never on the SSH command line or in Switchyard job records. This includes credentials even though credential **files** are excluded from snapshots. Use a worker you trust with those variables. A job container receives its own `HOME`, `PATH`, `PWD` and temporary/cache paths. Switchyard keeps the worker's `USER` and `LOGNAME` for rootless Podman while passing the invoking identity to the job through an unlinked private file descriptor. Commands can still print environment values into their output logs.

`switchyard run` preserves your current directory within the repo. It includes tracked files, their edits and deletions, and non-ignored new files. It omits Git-ignored files. For `jj`, files that remain ignored or oversized need an explicit path relative to the repo root:

```sh
switchyard run --include test/fixtures/ignored-input.dat -- pnpm test
```

Private paths such as `.env`, `.lore.md`, SSH keys, cloud credentials, `.opencode/`, and `.pnpm-store/` are excluded. Included symlinks, unresolved conflicts, and included files over 128 MiB fail before transfer. No project history or Git metadata is uploaded. Source changes detected during capture fail the run. Switchyard sends a selected-file snapshot, then synchronizes changed files into a verified worker mirror; each job receives its own writable copy. The worker retains finished job logs and patches for seven days and prunes them on the next run.

To test without SSH, use the built-in local backend with storage outside the source repo:

```sh
switchyard preview --repo /absolute/project/path
switchyard run --backend local --repo /absolute/project/path -- pnpm test
```

The local backend does not offload CPU. A stopped remote worker takes time to start; Switchyard has no cold-start or under-ten-second guarantee.

## DevPod

DevPod can create, stop and restart a [devcontainer workspace](https://devpod.sh/docs/developing-in-workspaces/what-are-workspaces). A future Switchyard adapter could ask DevPod to manage worker lifecycle, then measure time from dispatch to first command. DevPod does not replace Switchyard's snapshot protocol. Keep SSH host-key verification enabled when evaluating its connection.

## Develop

```sh
go test ./...
go vet ./...
```

The tests use disposable worktrees and private worker directories. The image tests need a pinned local image and a runtime visible to the test directory; ordinary tests use no cloud resources.

## Execution core (Linux preview)

The client needs Git and rsync (`/usr/bin/rsync` on SSH workers), `jj` locally for jj projects, and OpenSSH for SSH backends. It needs no container runtime to dispatch to a remote worker. Private data lives in `~/.local/share/compute-mesh-go/` by default; set `MESH_GO_HOME` to use a separate private directory.

```sh
# Local worker for a disposable project; storage must be outside the worktree.
switchyard preview --repo /path/to/project
switchyard run --backend local --repo /path/to/project -- go test ./...
switchyard proxy --backend local --repo /path/to/project -- go test ./...
switchyard jobs
switchyard status JOB_ID
switchyard logs JOB_ID
switchyard cancel JOB_ID

# On an operator-controlled Linux worker, after copying the binary there:
/absolute/path/switchyard worker --root /private/worker/path init

# Locally, after pinning the worker's SSH host key through a trusted channel:
switchyard backend add --name worker1 --kind ssh --root /private/worker/path \
  --host worker.example.com --user ubuntu --identity /absolute/path/to/key \
  --known-hosts /absolute/path/to/pinned_known_hosts \
  --host-key-alias worker1 --worker-command /absolute/path/switchyard \
  --priority 10 --capability cpu
switchyard run --backend worker1 --repo /path/to/project -- go test ./...
```

On Linux, an image-enabled worker uses **rootless Podman without a daemon** when Podman is installed. Docker workers still work; set `MESH_CONTAINER_RUNTIME=docker` to choose them explicitly. The runtime and full image ID are recorded together so jobs never use an image ID from another runtime's storage. On a manually managed worker, install Podman, `catatonit` (needed for `--init`), `uidmap`, `passt`, and a working rootless user-namespace setup (or use Docker), then run `switchyard image build` and `switchyard image status` **as the worker account**. The build uses a digest-pinned Node 24 Debian base, installs pnpm 10.28.0 directly, and stores the finished image ID under that worker's private home. The tested Docker image uses about 0.34 GiB unpacked. GitHub's full hosted-runner VM recipe includes many unrelated SDKs and runtimes; its `ubuntu-slim` container recipe includes cloud CLIs but lacks Switchyard's pinned pnpm worker setup. Add other worker profiles when a measured project needs them. The build needs registry access during the first pull and build. Node jobs pin `"packageManager": "pnpm@10.28.0"` and a matching lockfile; npm jobs require `package-lock.json`. The worker binds each frozen checkout into a separate container; source changes never write back to the local worktree. Network access defaults to `none` for commands other than pnpm/npm setup; setup uses Docker bridge or rootless Podman's private `pasta` network. Set `MESH_WORKER_NETWORK=none|bridge` on the worker to change that policy. Only a source snapshot and its returned patch cross hosts, not a mounted host filesystem or per-job OCI layer.

The AWS lifecycle uses `switchyard aws up|status|setup|down|destroy`. New AWS workers bootstrap rootless Podman, `catatonit`, and a two-hour stop timer; `setup` transfers the Go binary over pinned SSH, builds the Node image **on the worker**, and registers its SSH backend. Always preview every selected and excluded path before dispatch, then stop the worker and confirm `switchyard aws status` says `stopped`; the disk remains billed until `destroy` confirms removal. A live London rootless-Podman run installed pnpm dependencies, forwarded stdin and the invocation environment, returned exit 37 with an unapplied patch, and left no job container running. Fresh provisioning and `destroy` still need an independent end-to-end test.

The SSH worker accepts requests on stdin, so commands containing quotes or spaces never enter the remote login shell's command line. SSH checks the pinned host key, refuses agent forwarding, and fails when the key changes. Local and SSH jobs both use the same source selection: tracked edits/deletions, non-ignored new files, and explicit `--include` inputs; `.env`, `.lore.md`, private keys, credentials and caches are excluded. `jj` file-list output is resolved from the worktree root, including when `--repo` points to another directory. One content-hashed manifest describes each frozen capture; the worker verifies every file before use. Independent runs receive different private checkouts. Worker edits return as an unapplied Git patch; sensitive paths created by the job are excluded from the patch. Commands get their exact exit status, logs have a 128-MiB cap and patches have a 64-MiB cap.

Go jobs receive the exported environment **at command invocation**, including variables set after the agent or shell started. Switchyard sends it as a bounded private worker request over stdin (inside SSH for remote workers), never in command arguments, the source snapshot, or job records. This includes credentials from the invoking process: use only a worker and adapter you trust with those credentials. For host-executed jobs, the worker replaces `PWD` with the checkout and forces `MESH_BACKEND=local`; other host-specific paths remain unchanged. In image-enabled jobs, Switchyard also replaces `HOME`, `PATH`, `TMPDIR`, `XDG_CACHE_HOME`, `XDG_CONFIG_HOME` and `XDG_DATA_HOME` with container paths. It pins `npm_config_store_dir` to the persistent worker cache outside the checkout so pnpm artifacts cannot enter a returned patch. The engine keeps the worker's `USER`, `LOGNAME`, and `XDG_RUNTIME_DIR`; it receives invoking identity variables through an unlinked private file descriptor and passes them into the job container. The engine rejects invocation variables that override its client configuration (such as `DOCKER_HOST` or `CONTAINER_HOST`), rather than sending a job to a different daemon; `MESH_CONTAINER_RUNTIME` is a worker setting, not a job setting. Other caller-supplied values remain in process environment, not container command arguments. Commands can still print their environment to the job log.

For `pnpm` and `npm` jobs, the worker finds a `package.json` and matching lockfile in the captured tree, then runs `pnpm install --frozen-lockfile --prefer-offline` or `npm ci --prefer-offline --no-audit --no-fund` inside each private checkout before the requested command. Its package cache stays on worker disk; `node_modules` stays out of snapshots and returned patches. Setup failures are recorded as `setup-failed`, with the setup exit code separate from the requested command's exit. The worker needs a compatible Node and package-manager version and access to any required registry.

### Command interception

`switchyard proxy --backend worker1 --repo /path/to/project -- pnpm test` acts as a command proxy: command stdout and stderr remain separate, stdin reaches the worker, and the proxy exits with the command's status. It prints no Switchyard progress messages into either command stream on success. `switchyard jobs` lists local job IDs and returned patch paths; use `switchyard status JOB_ID` and review the patch before applying any edits. For `--backend local`, the proxy directly replaces itself with the original executable without capturing a snapshot.

On **Linux x86-64**, `switchyard trace install` creates a private `bash` link under `MESH_GO_HOME/ptrace/bin/` (normally `~/.local/share/compute-mesh-go/ptrace/bin/`). With `MESH_TRACE_SHELL=1`, that link supervises noninteractive Bash `-c` commands. It runs the installed `/usr/bin/bash` without changing its bytes. Bash's external `execve` pauses at a seccomp/ptrace stop; Switchyard hands a previewed, simple external command to the existing worker proxy and keeps Bash's original resolved executable path in `argv[0]` for the PATH/inode check. There is no command-name or subcommand allowlist for this traced path. No marker means automatic placement runs locally; a forced backend without a marker fails for an otherwise routable command. The marker is the caller's assertion, **not** proof that Switchyard ran preview. Inspect every selected and excluded path before each dispatch:

```sh
switchyard preview --repo /absolute/project/path
# After inspecting every path, in an OpenCode shell-tool command rooted at that project:
MESH_TRACE_PREVIEWED=1 MESH_BACKEND=aws pnpm test
```

The link is opt-in through the OpenCode service's `SHELL` environment and `MESH_TRACE_SHELL=1`, with no `shell` override in `opencode.json`; restart the service to load these settings. Interactive/terminal Bash runs as ordinary `/usr/bin/bash`. The tracer only routes the foreground Bash process when its standard file descriptors still match the wrapper's: pipelines, redirections, background commands, and forked shell jobs retain Bash's behavior. Descendants are traced to keep inherited filters functional; a supervisor remains for background children after the foreground shell exits. This is a **bounded shell-tool path**, not general interception of other programs or interactive shell commands. `execveat` is observed but not replaced, and unsupported platforms fail `switchyard trace` explicitly. A forced PATH/inode mismatch fails before remote execution; automatic placement keeps Bash's selected executable local. An interrupted wrapper kills its supervised foreground tree; long-running detached children keep a supervisor until they exit. Terminal control and nested-shell routing are not established. Eight interleaved local trials of 30 `/usr/bin/true` calls measured 28 ms with Bash and 69 ms with the traced link (medians, including shell startup on this host); this does not measure remote dispatch. Revert by restoring `SHELL=/usr/bin/bash`, removing `MESH_TRACE_SHELL` from the service environment, and restarting OpenCode; the host Bash and login shell never change.

Automatic placement learns from **completed runs** in the exact working directory with the same resolved executable and argument list. It stores only a SHA-256 digest of those inputs, bounded wall/CPU times, and backend IDs in a private file; it never stores raw commands or arguments. Keep this file private: common arguments could be guessed from their digest. After at least two local and two remote samples, it compares median local and end-to-end remote time. It chooses a ready remote backend only when local CPU is under pressure and the measured remote time is at least 20% faster; otherwise it keeps the command local. New commands start locally unless you explicitly choose a backend after previewing. Remote samples require a named SSH/plugin worker; local worker fixtures do not establish a remote speedup. Worker image changes can invalidate timing history, so remove the private `observations/timings.json` file before relying on old samples after an image or toolchain upgrade. This is a first measured rule, not a learned estimate for unobserved argument combinations or a guarantee of lower startup time.

`switchyard hook install` creates private `PATH` shims for `pnpm`, `npm`, `go`, `cargo`, `pytest`, `uv`, and `make`, plus an experimental `meshell` entrypoint (`mesh-shell` remains an alias). The entrypoint handles the shell's `-c` invocation while leaving the harness's original command and permission scan intact. A Node `child_process.spawnSync(..., { shell: meshellPath })` integration test passes. **Do not set OpenCode's global `shell` setting to the `meshell` wrapper:** OpenCode also uses that setting for terminal and user-shell commands, where it selects different login-shell flags based on the executable's name. On this host, the service environment points `SHELL` to the private traced-Bash link above; after a restart, it affects terminal and user-shell selection as well as the shell tool. A narrower post-permission, pre-spawn shell-tool adapter remains an option for those paths.

The experimental shell entrypoint recognizes single literal calls to the named `pnpm`, `npm`, `go`, `cargo`, `pytest`, `uv`, and `make` shims, including absolute tool paths; it does not filter by subcommand. It rejects shell operators, quotes, expansions, redirections and multiline scripts from remote routing. Interactive invocations pass through to Bash; commands typed inside Bash therefore stay local. Routing those commands requires integration at Bash's external-command launch boundary, not another parser in `meshell`. The entrypoint uses the measured placement rule above for automatic routing; without a per-command preview marker or sufficient observations it runs locally. Set `MESH_BACKEND=worker1` to force a command to a named backend, or `MESH_BACKEND=local` to force the original shell. Remote absolute tool paths resolve to the worker tool of the same name: pin and verify toolchain versions before relying on equivalent behavior. `switchyard run --require gpu -- ...` selects only an explicitly GPU-capable backend; advertising a capability does not install a GPU or its drivers.

For an **opt-in interactive Bash prototype**, `scripts/build-mesh-bash.sh` builds a pinned GNU Bash 5.2.37 with `scripts/bash-5.2.37-mesh.patch` and prints its private `bin/bash` path. The script keeps the downloaded source and license beside the binary. Launch that Bash from a disposable Git or `jj` worktree with `MESH_BASH_PROXY` set to the absolute `switchyard` binary and `MESH_BACKEND` set to a configured backend, for example:

```sh
scripts/build-mesh-bash.sh
scripts/test-mesh-bash.sh /path/printed/by/build-script/bin/bash "$HOME/.local/bin/switchyard"
MESH_BASH_PROXY="$HOME/.local/bin/switchyard" MESH_BACKEND=worker1 \
  /path/printed/by/build-script/bin/bash --noprofile --norc -i
# Inside that Bash: cd subproject && pnpm test
```

This hook runs after Bash expands a command and resolves functions and builtins. Its opt-in source patch recognizes the named tool shims above, including commands typed at the prompt or reached through `cd subproject && ...`; Switchyard does not filter their subcommands for placement. Bash keeps redirections, pipelines, background commands, command/process substitutions, parenthesized subshells, `command -p`, and explicit tool paths local while their remote semantics remain unverified. Exported variables and temporary `NAME=value pnpm test` assignments reach the worker with the command. Bash passes its resolved command path to Switchyard: if it differs from the executable on `PATH`, automatic placement runs Bash's chosen executable locally and a forced remote placement fails. The hook also leaves `MESH_BACKEND=local` local; unsetting `MESH_BASH_PROXY` disables it. A remote worker does not yet provide a full PTY, and non-exported shell variables, functions, aliases and other shell state cannot transfer to a worker. This source-patched Bash remains a separate opt-in experiment; the service environment no longer points to it. The `--shell` flag on `switchyard hook` permits commands from this patched Bash to route with terminal stdin; ordinary `PATH` shims still run terminal commands locally.

### Compiled-binary syscall interception (investigation)

The patched Bash above changes **source code before compilation**. It is a separate experiment from intercepting an **already compiled** shell or agent at its `execve`/`execveat` boundary. Do not treat rebuilding a shell on demand as the compiled-binary approach.

One approach is to start the existing executable under a supervisor, trace its children, and stop each tracee before an eligible `execve` or `execveat`. The bounded Linux x86-64 Bash shell-tool implementation above proves a replacement for simple `execve` calls: it preserves arguments, environment, streams, exit status, isolated worker edits, and local fallback in its fixture. Tracing is not rewriting the executable on disk, and attaching to an unrelated already-running agent is a separate permissions problem. A supervisor must intercept the syscall **before** successful exec; an event delivered only after exec is too late to substitute that launch. Seccomp user notification can allow or deny an exec but cannot simply return a different program in its place. A disposable Lore test benchmark confirmed `strace --seccomp-bpf` caught both syscalls, but CPU contention made the three-run test-suite overhead inconclusive. On 150 tiny external commands, seven-run medians were 166 ms without tracing, 217 ms with exec-filtered `strace`, and 370 ms with ordinary syscall tracing; these timings include `strace` startup and do not measure Switchyard's supervisor.

A different approach is to rewrite a **private copy** of a compiled executable, inserting a detour at its exec call site or a dynamic `execve` linkage point. This depends on the exact binary and build ID; a dynamic-library detour misses static binaries and direct syscalls. On this host's exact `/usr/bin/bash` build (SHA-256 `bc5945feb8bd26203ebfafea5ce1878bb2e32cb8fb50ab7ae395cfb1e1aaaef1`), `shell_execve` has one `execve@plt` call at `0x51560`. A disposable x86-64 detour in unused executable padding redirected `/usr/bin/true` to `/usr/bin/false`, then a second detour passed named eligible commands, Bash's resolved path, the original arguments and invocation environment to `switchyard hook --shell`. In an isolated local-worker fixture, non-interactive and typed interactive commands preserved stdin, stdout, stderr and exit 37, returned an unapplied worker-edit patch, and left the source files unchanged. An absent or invalid configured proxy failed closed for a forced backend; an unrelated external command stayed local. An early version also launched the Go proxy for ineligible `pnpm --version` calls: seven-run medians for 30 calls were 34.1 ms with Bash, 123.4 ms with the detour, and 49.3 ms with exec-filtered `strace`. After a small eligibility check in the detour, a second seven-run trial measured 40.2, 39.9 and 61.5 ms respectively. These short local runs vary with host load; they do not measure Switchyard dispatch. The original Bash was never changed, and undoing the private patch reproduced its bytes exactly. The test scripts and results are under `/tmp/opencode/mesh-binary-proxy-20261008/`.

**Do not install this binary detour as a general shell hook.** Its `shell_execve` call site has no reliable access to Bash's redirection, pipeline, background or subshell flags, so it cannot retain the source-patched hook's conservative guards. It does not intercept other executables' syscalls, `execveat`, or arbitrary Bash builds. A production binary patcher would need a verified earlier hook point or equivalent context checks, plus coverage for process groups, cancellation, terminals and failure paths. Keep the trial private and opt-in; never replace `/usr/bin/bash`, the login shell, or an OpenCode shell setting with it. The separate ptrace shell-tool path is bounded as described above.

For harnesses without a configurable command shell, prepend the shim directory to their **launch environment's** `PATH`, then restart the harness. The shims `exec` the original binary for unrelated commands, interactive commands and unavailable remote capacity; eligible `pnpm`/`npm` jobs now install dependencies inside the job checkout. `PATH` interception only catches executables resolved by name, while a configured shell also sees eligible absolute paths. Neither intercepts commands that bypass the harness's configured shell (such as a separate terminal/PTY tool). Linux `fanotify` and seccomp user notification can hold or deny an `exec`, but cannot replace it with a remote process while retaining its PID, open file descriptors, signal and exit semantics. No shell or shim is automatically enabled for a running agent.

### Opt-in CWD container shell prototype

`meshell` can also start a Podman or Docker container for its current directory. Set `MESH_CONTAINER_IMAGE` to a **locally cached image pinned by digest**; when unset, `meshell` retains the command-routing behavior described above. On Linux, Podman takes precedence when installed; use `MESH_CONTAINER_RUNTIME=podman|docker` to select one explicitly. By default, the first invocation creates a persistent container keyed by the canonical CWD and UID; later invocations at that exact CWD attach to it, even while an earlier command is still running. `MESH_CONTAINER_MODE=isolated` instead starts a fresh, automatically removed container for one invocation. Both modes bind-mount the CWD at its original absolute path and start `/bin/bash` there. Local writes through that mount change the live worktree. The shared container keeps a private home at `MESH_GO_HOME/containers/mesh-cwd-*/home`; the isolated container gets a temporary home. Stop a shared container before switching runtimes.

For a disposable project on this host, after `switchyard hook install`:

```sh
podman pull docker.io/library/ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
export MESH_CONTAINER_RUNTIME=podman
export MESH_CONTAINER_IMAGE=docker.io/library/ubuntu@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55
cd /absolute/path/to/disposable/project
"$HOME/.local/share/compute-mesh-go/shims/meshell" -c 'pwd; id'
MESH_CONTAINER_MODE=isolated "$HOME/.local/share/compute-mesh-go/shims/meshell" -c 'pwd'
switchyard container status --cwd /absolute/path/to/disposable/project
switchyard container stop --cwd /absolute/path/to/disposable/project
```

The container uses the caller's UID/GID, drops Linux capabilities, enables `no-new-privileges`, limits process count, has **no network by default**, and never explicitly mounts a container control socket. Known socket parent directories are rejected as CWDs; inspect the chosen bind directory for other sockets or sensitive files before use. `MESH_CONTAINER_NETWORK=bridge` enables container egress (mapped to private `pasta` for rootless Podman); the prototype does not publish service ports. It forwards invocation environment values through the runtime client, naming variables without putting their values in command arguments. **The values, including exported credentials, are available to processes in the container.** `PWD` becomes the canonical CWD and `HOME` becomes `/home/mesh`; other host-specific paths may not exist inside the image. Docker must see the bind source in its own mount namespace—tool-sandbox `/tmp` directories may not be visible to the daemon. A change of image digest, platform, or network setting requires stopping the existing CWD sandbox first; `container stop` works even after its image has been removed. It retains the private home for explicit review or reuse.

The Ubuntu CWD image proves local sharing and isolation; Ubuntu alone does not contain Switchyard's patched Bash, Go CLI, OpenCode, Node, `jj`, or project dependencies. Commands inside it do not yet route to remote Switchyard workers. The separate Node worker image runs frozen Go jobs and returns reviewed patches. `meshell` does not automatically use the same image as that worker: matching base recipes are not proof of byte-identical images. Sharing an exact OCI artifact across hosts needs a verified registry or image export/import path. A remote worker cannot bind-mount the client's CWD, and image commits omit bind-mounted files. OCI layers suit the shared toolchain, not per-edit synchronization: a changed file enters a layer in full. A future agent-server image must install OpenCode and the routing shell inside that same image and mount only the workspace and the state it needs. The current `opencode.service` starts at `/home/byk/Code`, not at an individual project root; keying it by that CWD would expose the entire Code tree and would not match a shell launched in a project subdirectory. The live service and its configured Bash have **not** been moved into this prototype. Shell-only containers do not confine OpenCode or tools that run outside them; access to the writable CWD mount and forwarded environment remains intentional.

### Third-party adapter contract and pooling

`switchyard backend add --name provider --kind plugin --root /private/worker/path --worker-command /absolute/path/to/adapter` registers a locally installed executable. It receives `worker --root ROOT ready|prepare|status|logs|patch|cancel` with the same flags as the built-in worker. For execution it receives `worker --root ROOT wire`, then a newline-terminated JSON request with `args` and base64-encoded `environment` entries on stdin, followed by the command's stdin. The adapter must pass the request and remaining stdin to the worker without logging or storing the environment. `prepare` creates the private incoming directory. For transfers it receives `sync --root ROOT --project ID --job ID --source ABSOLUTE_FROZEN_TREE`, and must copy the source tree to `ROOT/projects/ID/jobs/ID/incoming/files/` before returning success. The adapter must print only the requested worker output, preserve the declared command's exit status, and keep logs/edits available to later `status`/`logs`/`patch` calls. `ready` must be read-only. This contract allows an adapter to choose its own transport or lease a VM while the CLI keeps snapshot and result rules stable. Backends advertise capabilities and a priority; current readiness is a yes/no probe, not a provider lease or a resource reservation.

For a hosted service, keep a small pool of prepared workers per image/capability profile and place isolated jobs on capacity already paid for. Give each job a lease, per-job CPU/memory/runtime limits, a private writable checkout, and a fixed snapshot ID. Queue when the pool is full; scale it down after measured idle time. Meter occupied and reserved capacity, persistent disks, transfer and startup separately. A stopped VM with a retained disk still costs money; no pool design makes a cold worker ready in under ten seconds by itself. [exe.dev's pool documentation](https://exe.dev/docs/pools.md) gives a concrete business model: a pool reserves CPU/RAM, is billed even while empty, and can hold many VMs without adding compute charges per VM; disk and outbound bandwidth have separate allowances and overage. Its Work plan lists $75/month with 2 vCPU/4 GiB included, then $0.105 per extra 2-vCPU/4-GiB unit-hour. This shows how to sell reserved shared capacity instead of one always-on VM per user; it does not establish exe.dev's costs, margins or the economics of this prototype.

The prototype keeps the latest worker tree for rsync delta transfer, copies each job into a separate checkout, and returns a frozen snapshot/result record. It reclaims older mirrors after publishing the latest one, then prunes finished worker jobs and local records older than seven days when another job runs. The checkout has no `.git` or `.jj`; commands needing repository history require a prepared environment. `MESH_DEBUG=1` emits fixed phase names, results and elapsed times without paths, command arguments, credentials or raw cloud responses. Benchmark capture, transfer, checkout, first useful command, and end-to-end completion before committing to a storage/worker pool design. Larger trees still require a Git-pack comparison; a private object transfer may beat filesystem copies. Neither this Go prototype nor shell interception establishes an under-ten-second dispatch guarantee. Only the AWS adapter exists among the requested cloud integrations; its reused-worker job path was verified in London, while fresh provisioning and permanent destruction remain untested.
