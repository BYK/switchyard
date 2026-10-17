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
| `BenchmarkPatchCollection` | Unchanged, small-text-edit and binary-edit patch collection; paired full-tree and selective collectors on identical inputs; checkout preparation and outer job cleanup excluded |
| `BenchmarkDispatchArtifacts` | Actual local-provider CLI dispatch, capture, mirror preparation, rsync, command execution, automatic ignored-output return, and worker cleanup |

Fixtures contain 128 source-like text files totaling 2 MiB, four distinct binary files totaling 64 MiB, or 2,048 small text files totaling 8 MiB. The dependency capture fixture adds 8 MiB under excluded `node_modules`; it checks selection cost, not dependency installation. Inputs are synthetic. Cold/warm dispatch describes worker mirror state, not the OS page cache. The small-edit dispatch modifies input after an untimed priming run. The command writes an ignored artifact and checks that it reaches the invoking worktree before dispatch returns.

Each CI case runs three one-iteration samples. These are exploratory timings on shared runners, not performance thresholds. `proc-write-B/op` comes from `/proc/self/io` and includes accounted child-process writes after waiting; it is not peak disk usage, exclusive extent usage or network bytes. Buffered writeback and FUSE daemon accounting can differ. Filesystem tests and benchmarks operate entirely on disposable fixture directories.

## Selective patch collection

Workers automatically compare checkout content hashes, sizes and executable modes with the input manifest verified before execution. Only added, changed and deleted files enter a temporary Git repository. Unchanged source files are still scanned and hashed, but are no longer copied into Git objects. An unchanged checkout produces an empty patch. New files obey checkout ignore rules; explicitly captured files remain tracked even when ignored. Private paths and artifact roots stay excluded.

Changed preimages and postimages are copied or cloned into private staging and checked against their scanned hashes before Git reads them. Staging and its Git objects are removed when collection returns, including on failure. A failed collection retains the job's original input and checkout through the existing recovery mechanism. Git attributes cannot run content filters, and binary patches retain the existing transport format. Collection is still a post-command operation, not an atomic snapshot of concurrently changing background processes.

The test-only `full-tree` collector preserves the implementation from PR #14 as a compatibility and performance baseline. `BenchmarkCheckoutRoundTrip/…/auto-full-tree` and `/auto` compare complete loops with the same automatic checkout copying. `BenchmarkPatchCollection` isolates collection; selective staging cleanup is included, while legacy Git-data cleanup is deferred to outer job cleanup. This comparison therefore does not hide the selective collector's cleanup cost. Client-side source-patch application still stages the full captured tree; this optimization only changes worker collection.

## FUSE scope

The test-only overlay uses an immutable lower tree and a separate writable upper/work directory for each view. Tests cover contents, executable modes, additions, deletion whiteouts, result collection, 32 simultaneous views, and removing views while the remaining views stay readable. Parent and children in this experiment share a host and mount namespace. It does not prove cross-sandbox mount propagation, security isolation, live remote peeking, or crash recovery.

Production continues to use ordinary directories. Enabling FUSE in a worker would require explicit lower-tree lifetime management, container mount integration, failure recovery and a measured complete-job benefit. A faster mount by itself does not establish that benefit: file reads, metadata access and result collection also traverse FUSE.

## Measured results — 2026-10-17

[Workflow run 39063138467](https://github.com/BYK/switchyard/actions/runs/39063138467), code commit `3c7a3eef721aefacb4e2e64003b0bf12a151217b`. Go 1.22.12, Linux 6.17.0-1022-azure, fuse-overlayfs 1.13. The native ext4 runner used an Intel Xeon 6973P-C; the loopback Btrfs runner used an AMD EPYC 7763. Compare methods within a runner, not absolute times across filesystems. Values below are medians of three samples. [Raw ext4 samples](benchmarks/2026-10-17-ext4.txt) and [raw Btrfs samples](benchmarks/2026-10-17-btrfs.txt) retain all observations.

Btrfs capture reduces new large-file writes, but it does not remove hashing cost:

| Capture fixture | Copy (ms) | Automatic (ms) | Copy accounted writes (KiB) | Automatic accounted writes (KiB) |
| --- | ---: | ---: | ---: | ---: |
| 2 MiB text | 32.9 | 33.3 | 2136 | 2120 |
| 64 MiB binary | 162.3 | 173.1 | 65548 | 12 |
| 8 MiB / 2,048 files | 310.3 | 315.5 | 9108 | 9124 |

The binary capture saved approximately 64 MiB of accounted writes while taking about 7% longer. This is a write-amplification improvement, not a demonstrated latency improvement. Small captures use the ordinary copy+hash path; the 1 MiB cutoff is conservative, not a tuned optimum. On ext4, the 64 MiB capture took about 110 ms with either method and wrote the same number of bytes. An initial ext4 fallback regression was traced to truncating an already-empty destination; avoiding that truncate restored the normal copy behavior.

Complete checkout → command → patch collection → cleanup:

| Storage | Fixture | Automatic (ms) | FUSE (ms) |
| --- | --- | ---: | ---: |
| ext4 | 2 MiB text | 42.6 | 61.0 |
| ext4 | 64 MiB binary | 1460.7 | 1481.6 |
| ext4 | 2,048 files | 284.3 | 375.9 |
| Btrfs | 2 MiB text | 101.6 | 115.6 |
| Btrfs | 64 MiB binary | 2070.5 | 2094.4 |
| Btrfs | 2,048 files | 955.1 | 1005.6 |

FUSE did not improve the complete loop in these samples. On Btrfs with 2,048 files, preparation fell from 146 ms to 11 ms, but patch collection rose from 667 ms to 874 ms. On the binary fixture, patch collection consumed roughly 98–99% of automatic checkout-loop time. Reusing verified base Git objects/indexes or reducing result scanning is a more promising next experiment than enabling production FUSE. The ordinary `io.Copy` baseline's low preparation cost and write counts are consistent with Btrfs kernel copy offloading. The ext4 text/copy samples varied substantially (44–739 ms); they do not support a claimed copy-versus-reflink speedup.

The real local-provider dispatch benchmark (including automatic artifact return) measured:

| Storage | Cold (ms) | Warm unchanged (ms) | Warm small edit (ms) |
| --- | ---: | ---: | ---: |
| ext4 | 136.8 | 133.3 | 136.2 |
| Btrfs | 277.1 | 247.6 | 283.5 |

These dispatch values establish a reproducible local baseline, not a before/after comparison or a remote-cloud latency claim. FUSE was only used in the checkout experiment. FUSE isolation and the 32-view lifecycle test passed on both runners; required reflink isolation passed on Btrfs.

## Selective collection results — 2026-10-17

[Workflow run 39067182231](https://github.com/BYK/switchyard/actions/runs/39067182231), code commit `e0cd37c3539afd65732997af8aec9a456f84ba19`. Go 1.22.12, Linux 6.17.0-1022-azure, fuse-overlayfs 1.13-dev. The native ext4 runner used an AMD EPYC 7763; the loopback Btrfs runner used an AMD EPYC 9V45. These are paired old/new medians of three samples within each runner, not comparisons with the earlier run above. [Raw ext4 samples](benchmarks/2026-10-17-patch-ext4.txt) and [raw Btrfs samples](benchmarks/2026-10-17-patch-btrfs.txt) include every observation.

Complete checkout → README edit → patch collection → cleanup, using automatic checkout copying for both collectors:

| Storage | Fixture | Full-tree collector (ms) | Selective collector (ms) | Speedup |
| --- | --- | ---: | ---: | ---: |
| ext4 | 2 MiB text | 81.4 | 31.7 | 2.6× |
| ext4 | 64 MiB binary | 2083.1 | 86.7 | 24.0× |
| ext4 | 2,048 files | 693.2 | 298.5 | 2.3× |
| Btrfs | 2 MiB text | 69.1 | 29.3 | 2.4× |
| Btrfs | 64 MiB binary | 1423.6 | 68.0 | 20.9× |
| Btrfs | 2,048 files | 622.5 | 274.9 | 2.3× |

Isolated collection on the 64 MiB binary tree:

| Storage | Edit | Full-tree (ms) | Selective (ms) |
| --- | --- | ---: | ---: |
| ext4 | None | 1990.0 | 49.2 |
| ext4 | Small README edit | 1992.4 | 62.2 |
| ext4 | Edit one 16 MiB binary | 3230.0 | 1825.2 |
| Btrfs | None | 1404.8 | 48.4 |
| Btrfs | Small README edit | 1395.0 | 58.9 |
| Btrfs | Edit one 16 MiB binary | 2265.2 | 1333.7 |

Unchanged collection produced zero accounted writes on both runners. Small-edit collection fell from about 64 MiB to 60 KiB of accounted writes on ext4 and 68 KiB on Btrfs. The complete binary checkout loop fell from about 128 MiB to 64 MiB on ext4 and from about 64 MiB to 72 KiB on Btrfs. The changed binary case remains expensive because the collector must freeze and encode both versions of that file; its improvement was about 1.7–1.8×, rather than the much larger small-edit gain.

With selective collection, FUSE complete-loop medians were 56.5 / 106.9 / 501.7 ms on ext4 and 40.5 / 78.7 / 291.1 ms on Btrfs for text / binary / many-files respectively. FUSE still did not beat automatic ordinary checkouts in these samples, so it remains experimental.

The real local-provider artifact dispatch medians were 160.7 / 152.6 / 168.5 ms on ext4 and 199.6 / 159.2 / 156.1 ms on Btrfs for cold / warm / small-edit inputs. Btrfs cold samples varied from 191 to 806 ms. This dispatch benchmark exercises the optimized worker through the actual CLI but has no paired legacy-dispatch variant; it does not establish an end-to-end dispatch speedup. The paired checkout-loop benchmark also excludes client capture, transport, and client-side source-patch application. Full Go 1.22/stable race suites, patched Bash, actual FUSE mounts, and required Btrfs reflink checks passed.

## Reproduce

On Linux with Go, Git and rsync:

```sh
go test -race ./...
go test ./internal/switchyard -run '^$' \
  -bench 'Benchmark(SnapshotCapture|CheckoutRoundTrip|PatchCollection)$' -benchtime=1x -count=3
```

On a host with `/dev/fuse`, `fuse-overlayfs` and `fusermount3`, and a normal process namespace:

```sh
SWITCHYARD_TEST_FUSE=1 go test ./internal/switchyard -run 'Test(FUSE|Reflink)' -v
SWITCHYARD_TEST_FUSE=1 SWITCHYARD_BENCH_DISPATCH=1 \
  go test ./internal/switchyard -run '^$' \
  -bench 'Benchmark(SnapshotCapture|CheckoutRoundTrip|PatchCollection|DispatchArtifacts)$' \
  -benchtime=1x -count=3
```

Set `TMPDIR` to a private directory on the filesystem being measured. Set `SWITCHYARD_REQUIRE_REFLINK=1` when a missing reflink implementation should fail rather than skip the direct reflink test. See the workflow for disposable Btrfs setup.

Remaining work in #9 includes phase-specific network/seed measurements, exclusive and peak storage accounting, Btrfs subvolume snapshots, realistic dependency installation, and remote transport/compression comparisons. This experiment does not establish cloud speedups or justify zstd transport compression.

References: [Linux FICLONE contract](https://man7.org/linux/man-pages/man2/ioctl_ficlone.2.html), [fuse-overlayfs](https://github.com/containers/fuse-overlayfs), [process I/O accounting](https://man7.org/linux/man-pages/man5/proc_pid_io.5.html).
