package switchyard

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCopyFileFallbackAndStorageFailure(t *testing.T) {
	for _, failure := range []syscall.Errno{syscall.EOPNOTSUPP, syscall.EXDEV, syscall.EPERM, syscall.EINVAL, syscall.ENOTTY, syscall.ENOSYS, syscall.EIO, syscall.ENOSPC} {
		t.Run(failure.Error(), func(t *testing.T) {
			root := t.TempDir()
			source, dest := filepath.Join(root, "source"), filepath.Join(root, "dest")
			content := []byte("exact bytes\x00\xff")
			if err := os.WriteFile(source, content, 0o755); err != nil {
				t.Fatal(err)
			}
			err := copyFileWithClone(source, dest, 0o755, func(output, input *os.File) error {
				// Exercise a partial failed attempt, not just an untouched fd.
				if _, err := output.Write(bytes.Repeat([]byte("partial"), 20)); err != nil {
					t.Fatal(err)
				}
				if _, err := input.Seek(3, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				return failure
			})
			if failure == syscall.EIO || failure == syscall.ENOSPC {
				if !errors.Is(err, failure) {
					t.Fatalf("storage error was hidden: %v", err)
				}
				if _, err := os.Stat(dest); !os.IsNotExist(err) {
					t.Fatal("failed copy retained partial destination")
				}
			} else {
				if err != nil || !bytes.Equal(content, resultRead(t, dest)) {
					t.Fatalf("fallback changed the data: %v", err)
				}
				if info, err := os.Stat(dest); err != nil || info.Mode().Perm() != 0o755 {
					t.Fatal("fallback lost executable permissions")
				}
			}
			if !bytes.Equal(content, resultRead(t, source)) {
				t.Fatal("copy modified its input")
			}
		})
	}
}

func TestCopyTreeKeepsIndependentWritableFiles(t *testing.T) {
	for _, clone := range []struct {
		name string
		fn   fileCloner
	}{{"auto", cloneFile}, {"ordinary", nil}} {
		t.Run(clone.name, func(t *testing.T) {
			root := t.TempDir()
			source, first, second := filepath.Join(root, "base"), filepath.Join(root, "first"), filepath.Join(root, "second")
			artifactWrite(t, source, "nested/file", "original", 0o755)
			for _, dest := range []string{first, second} {
				if err := copyTreeWithClone(source, dest, clone.fn); err != nil {
					t.Fatal(err)
				}
			}
			baseInfo, _ := os.Stat(filepath.Join(source, "nested", "file"))
			copyInfo, _ := os.Stat(filepath.Join(first, "nested", "file"))
			if os.SameFile(baseInfo, copyInfo) {
				t.Fatal("writable checkout is a hard link to the base")
			}
			artifactWrite(t, first, "nested/file", "first write", 0o600)
			if err := os.RemoveAll(first); err != nil {
				t.Fatal(err)
			}
			artifactWrite(t, source, "nested/file", "base write", 0o600)
			if got := string(resultRead(t, filepath.Join(second, "nested", "file"))); got != "original" {
				t.Fatalf("another checkout or cleanup changed a frozen copy: %q", got)
			}
		})
	}
}

func TestCaptureCloneAndFallbackHaveIdenticalSnapshotIdentity(t *testing.T) {
	root, home := fixture(t), t.TempDir()
	artifactWrite(t, root, "binary", strings.Repeat("\x00\xffbinary", 1<<17), 0o755)
	plain, _, err := captureFilesWithClone(root, nil, home, nil)
	if err != nil {
		t.Fatal(err)
	}
	auto, _, err := captureFiles(root, nil, home)
	if err != nil || auto.Snapshot != plain.Snapshot {
		t.Fatalf("copy strategy changed snapshot identity: %v", err)
	}
	artifactWrite(t, root, "binary", "local edit", 0o600)
	for _, captured := range []capture{plain, auto} {
		if _, err := verifyTree(captured.Tree); err != nil {
			t.Fatalf("live source write changed its frozen capture: %v", err)
		}
	}
}

func TestReflinkIsolation(t *testing.T) {
	root := t.TempDir()
	input, err := os.Create(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	content := bytes.Repeat([]byte("immutable base\n"), 1024)
	if _, err := input.Write(content); err != nil {
		t.Fatal(err)
	}
	output, err := os.Create(filepath.Join(root, "clone"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	if err := cloneFile(output, input); err != nil {
		if os.Getenv("SWITCHYARD_REQUIRE_REFLINK") == "1" {
			t.Fatalf("required reflink support is unavailable: %v", err)
		}
		t.Skipf("filesystem does not support FICLONE: %v", err)
	}
	if _, err := output.WriteAt([]byte("changed"), 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resultRead(t, input.Name()), content) {
		t.Fatal("clone write changed shared base")
	}
	if _, err := input.WriteAt([]byte("source change"), 4096); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resultRead(t, output.Name())[4096:], content[4096:]) {
		t.Fatal("source write changed clone")
	}
}
