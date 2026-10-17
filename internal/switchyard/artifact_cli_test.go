package switchyard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArtifactCLI(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	executable := binary(t)
	for _, sample := range []struct{ action, mode, state string }{
		{"proxy", "", "applied"}, {"run", "", "applied"}, {"hook", "", "applied"},
		{"proxy", "conflict", "conflict"}, {"proxy", "transport", "failed"}, {"proxy", "truncated", "failed"},
		{"proxy", "tampered", "failed"}, {"proxy", "collect-fails", "failed"}, {"proxy", "patch-fails", "applied"},
		{"proxy", "review", "review"}, {"run", "legacy", ""},
	} {
		t.Run(sample.action+"/"+sample.mode, func(t *testing.T) {
			root, home := fixture(t), t.TempDir()
			t.Setenv("SWITCHYARD_HOME", home)
			t.Setenv("SWITCHYARD_RETURN_MODE", "")
			t.Setenv("SWITCHYARD_OUTPUTS", "")
			t.Setenv("SWITCHYARD_TRACE_HANDOFF", "1")
			t.Setenv("SWITCHYARD_BACKEND", "auto")
			t.Setenv("ARTIFACT_TEST_MODE", sample.mode)
			t.Setenv("RESULT_SOURCE", root)
			count := filepath.Join(t.TempDir(), "executions")
			t.Setenv("RESULT_COUNT", count)
			artifactWrite(t, root, ".gitignore", ".env\ndist/\nreport.txt\n", 0o644)
			artifactWrite(t, root, "dist/old.txt", "old output", 0o644)
			artifactWrite(t, root, "report.txt", "old report", 0o644)
			_, patch := resultFixture(t, root, home, func(checkout string) {
				recoveryWrite(t, filepath.Join(checkout, "README.md"), "remote\n", 0o644)
			})
			patchPath := filepath.Join(t.TempDir(), "source.patch")
			recoveryWrite(t, patchPath, string(patch), 0o600)
			archive, _, _ := artifactBundleFixture(t)
			provider := resultAdapter(t, executable, patchPath, archive)
			worker := filepath.Join(t.TempDir(), "worker")
			if err := initWorker(worker); err != nil {
				t.Fatal(err)
			}
			if err := saveBackend(home, backendConfig{Name: "sandbox", Kind: "plugin", Root: worker, Command: provider}); err != nil {
				t.Fatal(err)
			}
			script := `printf x >> "$RESULT_COUNT"; printf 'remote\n' > README.md; mkdir -p dist/empty; printf '\000new output\377' > dist/new.bin; chmod +x dist/new.bin; printf 'complete report\n' > report.txt; printf 'output\n'; printf 'error\n' >&2; `
			if sample.mode == "conflict" {
				script += `printf local > "$RESULT_SOURCE/dist/old.txt"; `
			}
			script += "exit 37"
			args := []string{sample.action, "--backend", "sandbox", "--repo", root, "--output", "dist", "--output", "report.txt", "--", "sh", "-c", script}
			if sample.mode == "review" {
				t.Setenv("SWITCHYARD_RETURN_MODE", "patch")
			}
			if sample.action == "hook" {
				t.Setenv("SWITCHYARD_OUTPUTS", `["dist","report.txt"]`)
				t.Setenv("SWITCHYARD_TRACE_HANDOFF", "")
				t.Setenv("SWITCHYARD_BACKEND", "sandbox")
				args = []string{"hook", "--", "sh", "-c", script}
			}
			started := time.Now()
			stdout, stderr, status := recoveryCommand(t, root, executable, args...)
			elapsed := time.Since(started)
			if sample.mode == "legacy" {
				if status != 2 || !strings.Contains(stderr, "does not support") {
					t.Fatalf("legacy worker did not fail before dispatch: %d %q", status, stderr)
				}
				if _, err := os.Stat(count); !os.IsNotExist(err) {
					t.Fatal("unsupported worker ran command")
				}
				return
			}
			if status != 37 || !strings.Contains(stdout, "output\n") || string(resultRead(t, count)) != "x" {
				t.Fatalf("command was lost or replayed: %d %q %q", status, stdout, stderr)
			}
			if sample.action != "run" && stdout != "output\n" {
				t.Fatalf("command stdout changed: %q", stdout)
			}
			if sample.state == "failed" || sample.state == "conflict" {
				if !strings.Contains(stderr, "artifact return "+sample.state) {
					t.Fatalf("missing artifact failure: %q", stderr)
				}
			} else if stderr != "error\n" {
				t.Fatalf("command stderr changed: %q", stderr)
			}
			if sample.state == "applied" {
				if string(resultRead(t, filepath.Join(root, "dist", "new.bin"))) != "\x00new output\xff" || string(resultRead(t, filepath.Join(root, "report.txt"))) != "complete report\n" {
					t.Fatal("outputs did not return")
				}
				if _, err := os.Stat(filepath.Join(root, "dist", "old.txt")); !os.IsNotExist(err) {
					t.Fatal("stale output survived")
				}
				t.Logf("dispatch-to-usable-output latency (local provider fixture): %s", elapsed)
			} else {
				expected := "old output"
				if sample.mode == "conflict" {
					expected = "local"
				}
				if string(resultRead(t, filepath.Join(root, "dist", "old.txt"))) != expected || string(resultRead(t, filepath.Join(root, "report.txt"))) != "old report" {
					t.Fatal("failed output return changed existing artifacts")
				}
				if _, err := os.Stat(filepath.Join(root, "dist", "new.bin")); !os.IsNotExist(err) {
					t.Fatal("partial output was published")
				}
			}
			expectedSource := "remote\n"
			if sample.action == "run" || sample.mode == "review" || sample.mode == "patch-fails" {
				expectedSource = "original\n"
			}
			if string(resultRead(t, filepath.Join(root, "README.md"))) != expectedSource {
				t.Fatal("source and artifact return were not independent")
			}
			records, _ := filepath.Glob(filepath.Join(home, "runs", "*.json"))
			if len(records) != 1 {
				t.Fatal("missing local job record")
			}
			job := strings.TrimSuffix(filepath.Base(records[0]), ".json")
			record, err := loadLocalJob(home, job)
			if err != nil || record.ArtifactReturn != sample.state {
				t.Fatalf("incorrect artifact status: %+v %v", record, err)
			}
			if sample.state != "failed" {
				if _, err := os.Stat(filepath.Join(home, "runs", job+".artifacts.tar")); err != nil {
					t.Fatal("validated archive was not retained")
				}
			}
			out, _, code := recoveryCommand(t, root, executable, "jobs")
			if code != 0 || !strings.Contains(out, "artifacts="+sample.state) {
				t.Fatalf("jobs omitted artifact status: %s", out)
			}
			out, _, code = recoveryCommand(t, root, executable, "status", job)
			if code != 0 || !strings.Contains(out, "Worker artifacts: "+sample.state) {
				t.Fatalf("status omitted artifact return: %s", out)
			}
		})
	}
}
