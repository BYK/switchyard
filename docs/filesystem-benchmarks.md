# Filesystem copying and FUSE experiments

Switchyard automatically attempts Linux `FICLONE` when freezing selected source files of at least 1 MiB and copying mirror, checkout, or result-staging files. Capture keeps ordinary copy+hash for smaller files: reflinking dirty small files required extent flushes and was slower in the initial Btrfs experiment. Source and destination must be on a compatible filesystem. Unsupported filesystems, cross-device copies and restricted ioctl access fall back to the existing ordinary copy path. Linux amd64, arm64 and riscv64 use the ioctl; other targets use ordinary copying. I/O errors and exhausted storage fail the operation. A failed copy does not leave a partial destination file.

Clones have independent inodes, share unchanged extents, and isolate later writes in either direction. Capture hashes the frozen clone and retains the original source validation and size limits. Hashing and directory traversal still read the selected tree; reflinks do not make capture constant-time. Snapshot identity, network transfer and result-return behavior stay the same. The client source and `SWITCHYARD_HOME` must share a compatible filesystem for capture cloning; worker-local copies can benefit independently.

The baseline uses the original Go `io.Copy` path, which may itself use kernel copy offloading. Explicit reflinks need not improve every copy on every filesystem. No hard links, filesystem migrations or privileged production mounts are introduced.

## What the experiments measure

The `Filesystem experiments` workflow runs on every PR and main push, on an Ubuntu runner's native storage and a disposable loopback Btrfs filesystem. It requires real FICLONE support on Btrfs and runs real `fuse-overlayfs` mounts on both filesystems. The Btrfs image is unmounted afterward. Benchmark outputs are retained as workflow artifacts.

| Benchmark | Included work |
| --- | --- |
| `BenchmarkSnapshotCapture` | Selection, copy/clone, frozen-content hash and live-source validation; capture cleanup excluded |
| `BenchmarkCheckoutRoundTrip` | Copy/clone/FUSE mount, one file read and a README edit, actual Git patch collection, unmount and directory removal; individual phase timings included |
| `BenchmarkDispatchArtifacts` | Actual local-provider CLI dispatch, capture, mirror preparation, rsync, command execution, automatic ignored-output return, and worker cleanup |

Fixtures contain 128 source-like text files totaling 2 MiB, four distinct binary files totaling 64 MiB, or 2,048 small text files totaling 8 MiB. The dependency capture fixture adds 8 MiB under excluded `node_modules`; it checks selection cost, not dependency installation. Inputs are synthetic. Cold/warm dispatch describes worker mirror state, not the OS page cache. The small-edit dispatch modifies input after an untimed priming run. The command writes an ignored artifact and checks that it reaches the invoking worktree before dispatch returns.

Each CI case runs three one-iteration samples. These are exploratory timings on shared runners, not performance thresholds. `proc-write-B/op` comes from `/proc/self/io` and includes accounted child-process writes after waiting; it is not peak disk usage, exclusive extent usage or network bytes. Buffered writeback and FUSE daemon accounting can differ. Filesystem tests and benchmarks operate entirely on disposable fixture directories.

## FUSE scope

The test-only overlay uses an immutable lower tree and a separate writable upper/work directory for each view. Tests cover contents, executable modes, additions, deletion whiteouts, result collection, 32 simultaneous views, and removing views while the remaining views stay readable. Parent and children in this experiment share a host and mount namespace. It does not prove cross-sandbox mount propagation, security isolation, live remote peeking, or crash recovery.

Production continues to use ordinary directories. Enabling FUSE in a worker would require explicit lower-tree lifetime management, container mount integration, failure recovery and a measured complete-job benefit. A faster mount by itself does not establish that benefit: file reads, metadata access and result collection also traverse FUSE.

## Reproduce

On Linux with Go, Git and rsync:

```sh
go test -race ./...
go test ./internal/switchyard -run '^$' \
  -bench 'Benchmark(SnapshotCapture|CheckoutRoundTrip)$' -benchtime=1x -count=3
```

On a host with `/dev/fuse`, `fuse-overlayfs` and `fusermount3`, and a normal process namespace:

```sh
SWITCHYARD_TEST_FUSE=1 go test ./internal/switchyard -run 'Test(FUSE|Reflink)' -v
SWITCHYARD_TEST_FUSE=1 SWITCHYARD_BENCH_DISPATCH=1 \
  go test ./internal/switchyard -run '^$' \
  -bench 'Benchmark(SnapshotCapture|CheckoutRoundTrip|DispatchArtifacts)$' \
  -benchtime=1x -count=3
```

Set `TMPDIR` to a private directory on the filesystem being measured. Set `SWITCHYARD_REQUIRE_REFLINK=1` when a missing reflink implementation should fail rather than skip the direct reflink test. See the workflow for disposable Btrfs setup.

Remaining work in #9 includes phase-specific network/seed measurements, exclusive and peak storage accounting, Btrfs subvolume snapshots, realistic dependency installation, and remote transport/compression comparisons. This experiment does not establish cloud speedups or justify zstd transport compression.

References: [Linux FICLONE contract](https://man7.org/linux/man-pages/man2/ioctl_ficlone.2.html), [fuse-overlayfs](https://github.com/containers/fuse-overlayfs), [process I/O accounting](https://man7.org/linux/man-pages/man5/proc_pid_io.5.html).
