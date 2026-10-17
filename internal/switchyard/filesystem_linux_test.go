package switchyard

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Experimental only: production checkouts remain ordinary directories. The
// lower tree must remain immutable for the complete lifetime of this mount.
func fuseCheckout(base, target, state string) (func() error, error) {
	if strings.ContainsAny(base+target+state, ",:\\") {
		return nil, fmt.Errorf("unsupported FUSE experiment path")
	}
	for _, path := range []string{target, filepath.Join(state, "upper"), filepath.Join(state, "work")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}
	log, err := os.Create(filepath.Join(state, "fuse.log"))
	if err != nil {
		return nil, err
	}
	defer log.Close()
	options := "lowerdir=" + base + ",upperdir=" + filepath.Join(state, "upper") + ",workdir=" + filepath.Join(state, "work")
	cmd := exec.Command("fuse-overlayfs", "-f", "-o", options, target)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	var cleanupErr error
	unmount := func() error {
		out, err := exec.Command("fusermount3", "-u", "--", target).CombinedOutput()
		if err != nil {
			_ = cmd.Process.Kill()
			if lazyErr := exec.Command("fusermount3", "-u", "-z", "--", target).Run(); lazyErr != nil {
				return fmt.Errorf("FUSE unmount failed: %v: %s", err, out)
			}
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		return nil
	}
	cleanup := func() error {
		once.Do(func() { cleanupErr = unmount() })
		return cleanupErr
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			content, _ := os.ReadFile(filepath.Join(state, "fuse.log"))
			return nil, fmt.Errorf("FUSE mount exited: %v: %s", err, content)
		case <-deadline.C:
			_ = cmd.Process.Kill()
			_ = exec.Command("fusermount3", "-u", "-z", "--", target).Run()
			<-done
			return nil, fmt.Errorf("FUSE mount did not become ready")
		case <-tick.C:
			var info syscall.Statfs_t
			if syscall.Statfs(target, &info) == nil && info.Type == 0x65735546 {
				return cleanup, nil
			}
		}
	}
}

func TestFUSECheckoutIsolation(t *testing.T) {
	if os.Getenv("SWITCHYARD_TEST_FUSE") != "1" {
		t.Skip("set SWITCHYARD_TEST_FUSE=1 on a host with /dev/fuse and fuse-overlayfs")
	}
	root := t.TempDir()
	base, view := filepath.Join(root, "base"), filepath.Join(root, "view")
	artifactWrite(t, base, "nested/edit", "original", 0o644)
	artifactWrite(t, base, "remove", "keep in base", 0o644)
	cleanup, err := fuseCheckout(base, view, filepath.Join(root, "overlay"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	artifactWrite(t, view, "nested/edit", "changed", 0o755)
	if err := os.Chmod(filepath.Join(view, "nested", "edit"), 0o755); err != nil {
		t.Fatal(err)
	}
	artifactWrite(t, view, "new", "new output", 0o644)
	if err := os.Remove(filepath.Join(view, "remove")); err != nil {
		t.Fatal(err)
	}
	if string(resultRead(t, filepath.Join(base, "nested", "edit"))) != "original" || string(resultRead(t, filepath.Join(base, "remove"))) != "keep in base" {
		t.Fatal("FUSE checkout changed immutable base")
	}
	if _, err := os.Stat(filepath.Join(base, "new")); !os.IsNotExist(err) {
		t.Fatal("FUSE creation escaped into base")
	}
	if info, err := os.Stat(filepath.Join(base, "nested", "edit")); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatal("FUSE chmod changed immutable base")
	}
	patch := filepath.Join(root, "changes.patch")
	if reason := collectPatch(base, view, patch); reason != "" {
		t.Fatal(reason)
	}
	data := resultRead(t, patch)
	for _, expected := range []string{"+changed", "+new output", "deleted file mode", "100755"} {
		if !bytes.Contains(data, []byte(expected)) {
			t.Fatalf("overlay result collection lost %q", expected)
		}
	}
}

func TestFUSEDynamicCheckouts(t *testing.T) {
	if os.Getenv("SWITCHYARD_TEST_FUSE") != "1" {
		t.Skip("FUSE experiment disabled")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base")
	artifactWrite(t, base, "state", "base", 0o644)
	var cleanups []func() error
	for i := 0; i < 32; i++ {
		view := filepath.Join(root, fmt.Sprintf("child-%02d", i))
		cleanup, err := fuseCheckout(base, view, view+"-overlay")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := cleanup(); err != nil {
				t.Error(err)
			}
		})
		cleanups = append(cleanups, cleanup)
		artifactWrite(t, view, "state", fmt.Sprintf("child %d", i), 0o644)
	}
	// An already-running parent reads 32 independent views without a fixed
	// startup mount list. Retire some children while the rest remain readable.
	for i, cleanup := range cleanups {
		view := filepath.Join(root, fmt.Sprintf("child-%02d", i))
		if got := string(resultRead(t, filepath.Join(view, "state"))); got != fmt.Sprintf("child %d", i) {
			t.Fatalf("child %d sees %q", i, got)
		}
		if i%2 == 0 {
			if err := cleanup(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := string(resultRead(t, filepath.Join(base, "state"))); got != "base" {
		t.Fatal("children modified shared base")
	}
}

type filesystemFixture struct {
	name, path  string
	count, size int
}

var filesystemFixtures = []filesystemFixture{
	{"text", "src", 128, 16 << 10},
	{"binary", "src", 4, 16 << 20},
	{"many-files", "src", 2048, 4096},
	{"dependencies", "node_modules", 2048, 4096},
}

func filesystemCorpus(tb testing.TB, sample filesystemFixture) string {
	tb.Helper()
	root := filepath.Join(tb.TempDir(), "source")
	if err := os.MkdirAll(filepath.Join(root, sample.path), 0o700); err != nil {
		tb.Fatal(err)
	}
	for name, value := range map[string]string{".gitignore": "node_modules/\ndist/\n.env\n", "README.md": "before\n", ".env": "excluded fixture\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	// Deterministic, non-sparse data. Binary files are incompressible; the other
	// fixtures contain source-like text. Each file differs to avoid Git blob dedup.
	// No source credentials or real repositories are used.
	data := make([]byte, sample.size)
	var value uint32 = 0x12345678
	for i := range data {
		if sample.name == "binary" {
			value ^= value << 13
			value ^= value >> 17
			value ^= value << 5
			data[i] = byte(value)
		} else {
			const line = "const value = 'synthetic source fixture';\n"
			data[i] = line[i%len(line)]
		}
	}
	for i := 0; i < sample.count; i++ {
		copy(data, fmt.Sprintf("// file %08d\n", i))
		if err := os.WriteFile(filepath.Join(root, sample.path, fmt.Sprintf("file-%04d", i)), data, 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		tb.Fatalf("init: %v %s", err, out)
	}
	return root
}

func BenchmarkSnapshotCapture(b *testing.B) {
	for _, sample := range filesystemFixtures {
		for _, method := range []struct {
			name  string
			clone fileCloner
		}{{"copy", nil}, {"auto", cloneFile}} {
			b.Run(sample.name+"/"+method.name, func(b *testing.B) {
				root, home := filesystemCorpus(b, sample), b.TempDir()
				var writeBytes int64
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					beforeIO := processWriteBytes()
					b.StartTimer()
					captured, _, err := captureFilesWithClone(root, nil, home, method.clone)
					if err != nil {
						b.Fatal(err)
					}
					b.StopTimer()
					writeBytes += processWriteBytes() - beforeIO
					if err := os.RemoveAll(filepath.Dir(captured.Tree)); err != nil {
						b.Fatal(err)
					}
					b.StartTimer()
				}
				b.ReportMetric(float64(writeBytes)/float64(b.N), "proc-write-B/op")
			})
		}
	}
}

// This measures checkout -> representative command -> patch export -> cleanup,
// including FUSE mount and unmount. It is not a network or cloud benchmark.
func BenchmarkCheckoutRoundTrip(b *testing.B) {
	for _, sample := range filesystemFixtures[:3] {
		for _, method := range []string{"copy", "auto", "fuse"} {
			b.Run(sample.name+"/"+method, func(b *testing.B) {
				if method == "fuse" && os.Getenv("SWITCHYARD_TEST_FUSE") != "1" {
					b.Skip("FUSE experiment disabled")
				}
				source := filesystemCorpus(b, sample)
				captured, _, err := captureFiles(source, nil, b.TempDir())
				if err != nil {
					b.Fatal(err)
				}
				base := captured.Tree
				if err := os.Remove(filepath.Join(base, manifestName)); err != nil {
					b.Fatal(err)
				}
				var setupTime, commandTime, resultTime, cleanupTime time.Duration
				var writeBytes int64
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					job := b.TempDir()
					checkout := filepath.Join(job, "checkout")
					beforeIO := processWriteBytes()
					b.StartTimer()
					started := time.Now()
					cleanup := func() error { return nil }
					if method == "fuse" {
						cleanup, err = fuseCheckout(base, checkout, filepath.Join(job, "overlay"))
					} else {
						var clone fileCloner
						if method == "auto" {
							clone = cloneFile
						}
						err = copyTreeWithClone(base, checkout, clone)
					}
					if err != nil {
						b.Fatal(err)
					}
					setupTime += time.Since(started)
					// Always tear down the mount before TempDir's recursive cleanup.
					active := cleanup
					b.Cleanup(func() {
						if active != nil {
							_ = active()
						}
					})
					started = time.Now()
					file, err := os.Open(filepath.Join(checkout, sample.path, "file-0000"))
					if err != nil {
						b.Fatal(err)
					}
					_, err = io.Copy(io.Discard, io.LimitReader(file, 64<<10))
					file.Close()
					if err != nil {
						b.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("after\n"), 0o644); err != nil {
						b.Fatal(err)
					}
					commandTime += time.Since(started)
					started = time.Now()
					if reason := collectPatch(base, checkout, filepath.Join(job, "result.patch")); reason != "" {
						b.Fatal(reason)
					}
					resultTime += time.Since(started)
					started = time.Now()
					if err := cleanup(); err != nil {
						b.Fatal(err)
					}
					active = nil
					if err := os.RemoveAll(job); err != nil {
						b.Fatal(err)
					}
					cleanupTime += time.Since(started)
					b.StopTimer()
					writeBytes += processWriteBytes() - beforeIO
					original, err := os.ReadFile(filepath.Join(base, "README.md"))
					if err != nil || string(original) != "before\n" {
						b.Fatal("checkout changed shared base")
					}
					b.StartTimer()
				}
				b.ReportMetric(float64(setupTime.Nanoseconds())/float64(b.N), "prepare-ns/op")
				b.ReportMetric(float64(commandTime.Nanoseconds())/float64(b.N), "command-ns/op")
				b.ReportMetric(float64(resultTime.Nanoseconds())/float64(b.N), "export-ns/op")
				b.ReportMetric(float64(cleanupTime.Nanoseconds())/float64(b.N), "cleanup-ns/op")
				b.ReportMetric(float64(writeBytes)/float64(b.N), "proc-write-B/op")
			})
		}
	}
}

func processWriteBytes() int64 {
	data, _ := os.ReadFile("/proc/self/io")
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "write_bytes:") {
			value, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "write_bytes:")), 10, 64)
			return value
		}
	}
	return 0
}

// Real local-provider round trip, including capture, mirror seeding, rsync,
// worker execution, automatic artifact return and cleanup. CI supplies a normal
// process namespace. Cold and warm measure worker-cache state, not OS page cache.
func BenchmarkDispatchArtifacts(b *testing.B) {
	if os.Getenv("SWITCHYARD_BENCH_DISPATCH") != "1" {
		b.Skip("set SWITCHYARD_BENCH_DISPATCH=1 on an unrestricted Linux host")
	}
	exe := filepath.Join(b.TempDir(), "switchyard")
	cmd := exec.Command("go", "build", "-o", exe, "../../cmd/switchyard")
	if out, err := cmd.CombinedOutput(); err != nil {
		b.Fatalf("build: %v %s", err, out)
	}
	for _, state := range []string{"cold", "warm", "small-edit"} {
		b.Run(state, func(b *testing.B) {
			root := filesystemCorpus(b, filesystemFixtures[0])
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				home := b.TempDir()
				init := exec.Command(exe, "backend", "add", "--name", "bench", "--kind", "local", "--root", filepath.Join(home, "worker"))
				init.Env = append(os.Environ(), "SWITCHYARD_HOME="+home)
				if out, err := init.CombinedOutput(); err != nil {
					b.Fatalf("backend: %v %s", err, out)
				}
				run := func() {
					cmd := exec.Command(exe, "proxy", "--backend", "bench", "--repo", root, "--", "sh", "-c", "mkdir -p dist; sha256sum src/file-0000 > dist/result")
					cmd.Env = append(os.Environ(), "SWITCHYARD_HOME="+home, "SWITCHYARD_RETURN_MODE=apply", "SWITCHYARD_OUTPUTS=")
					if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
						b.Fatalf("dispatch: %v %s", err, out)
					}
				}
				if state != "cold" {
					run()
				}
				if state == "small-edit" {
					file, err := os.OpenFile(filepath.Join(root, "src", "file-0000"), os.O_WRONLY, 0)
					if err != nil {
						b.Fatal(err)
					}
					_, err = file.WriteAt([]byte(fmt.Sprintf("edit %08d", i)), 0)
					file.Close()
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				run()
				b.StopTimer()
				if info, err := os.Stat(filepath.Join(root, "dist", "result")); err != nil || info.Size() == 0 {
					b.Fatal("artifact not usable")
				}
				b.StartTimer()
			}
		})
	}
}
