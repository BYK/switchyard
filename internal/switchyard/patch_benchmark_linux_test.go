package switchyard

import (
	"os"
	"path/filepath"
	"testing"
)

// Compare both collectors on the same host and deterministic inputs. Checkout
// preparation and outer job cleanup are excluded here; CheckoutRoundTrip
// includes them. The selective collector's private staging cleanup is included.
func BenchmarkPatchCollection(b *testing.B) {
	for _, sample := range filesystemFixtures[:3] {
		source := filesystemCorpus(b, sample)
		captured, _, err := captureFiles(source, nil, b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		if err := os.Remove(filepath.Join(captured.Tree, manifestName)); err != nil {
			b.Fatal(err)
		}
		for _, edit := range []string{"unchanged", "small-edit", "binary-edit"} {
			if edit == "binary-edit" && sample.name != "binary" {
				continue
			}
			for _, method := range []struct {
				name    string
				collect func(string, string, string, []fileEntry, ...string) string
			}{{"full-tree", collectPatchFullTreeBaseline}, {"selective", collectPatch}} {
				b.Run(sample.name+"/"+edit+"/"+method.name, func(b *testing.B) {
					var writeBytes int64
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						b.StopTimer()
						job := b.TempDir()
						checkout := filepath.Join(job, "checkout")
						if err := copyTree(captured.Tree, checkout); err != nil {
							b.Fatal(err)
						}
						if edit == "small-edit" {
							if err := os.WriteFile(filepath.Join(checkout, "README.md"), []byte("after\n"), 0o644); err != nil {
								b.Fatal(err)
							}
						} else if edit == "binary-edit" {
							file, err := os.OpenFile(filepath.Join(checkout, "src", "file-0000"), os.O_WRONLY, 0)
							if err != nil {
								b.Fatal(err)
							}
							_, err = file.WriteAt([]byte("changed binary"), 0)
							closeErr := file.Close()
							if err != nil || closeErr != nil {
								b.Fatalf("edit: %v %v", err, closeErr)
							}
						}
						beforeIO := processWriteBytes()
						b.StartTimer()
						if reason := method.collect(captured.Tree, checkout, filepath.Join(job, "result.patch"), captured.Manifest.Files); reason != "" {
							b.Fatal(reason)
						}
						b.StopTimer()
						writeBytes += processWriteBytes() - beforeIO
						if err := os.RemoveAll(job); err != nil {
							b.Fatal(err)
						}
						b.StartTimer()
					}
					b.ReportMetric(float64(writeBytes)/float64(b.N), "proc-write-B/op")
				})
			}
		}
	}
}
