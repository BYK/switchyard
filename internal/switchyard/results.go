package switchyard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var errResultConflict = errors.New("local files changed since capture; worker patch was not applied")

func applyResults(proxy bool) (bool, error) {
	switch os.Getenv("SWITCHYARD_RETURN_MODE") {
	case "":
		return proxy, nil
	case "apply":
		return true, nil
	case "patch":
		return false, nil
	default:
		return false, errors.New("SWITCHYARD_RETURN_MODE must be apply or patch")
	}
}

// Snapshot transport promises file bytes, not Git's cleaned representation.
// info/attributes takes precedence over project attributes, including CRLF,
// ident, encoding, and clean/smudge filters. Never run a repository's filters.
func configurePatchAttributes(gitDir string) error {
	return os.WriteFile(filepath.Join(gitDir, "info", "attributes"),
		[]byte("* -text -filter -ident -working-tree-encoding\n"), 0o600)
}

func applyGitPatch(index, gitDir, worktree string, patch []byte, options ...string) error {
	args := []string{"--git-dir=" + gitDir, "--work-tree=" + worktree,
		"apply", "--binary", "--whitespace=nowarn"}
	args = append(args, options...)
	cmd := exec.Command("git", append(args, "-")...)
	cmd.Dir = worktree
	cmd.Env = patchEnvironment(index)
	cmd.Stdin = bytes.NewReader(patch)
	// Git's output can contain file contents and private paths. Callers report
	// fixed diagnostics and retain the original patch instead.
	return cmd.Run()
}

func patchEnvironment(index string) []string {
	environment := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			environment = append(environment, entry)
		}
	}
	return append(environment, "GIT_INDEX_FILE="+index, "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_ATTR_NOSYSTEM=1")
}

// Return the complete patch, or leave it for review on a preflight conflict.
// The lock serializes Switchyard publishers sharing this state directory.
// External editors do not take it: this is not a multi-file filesystem
// transaction, and arbitrary concurrent writes or I/O failures can still race
// Git's final preimage checks. Never retry an application after a write failure.
func returnWorkerEdits(home, root string, captured capture, patch []byte, outputs ...string) error {
	if len(patch) == 0 || len(patch) > maxPatchSize {
		return errors.New("worker patch is empty or too large")
	}
	lockDir := workspaceLockDir(home, root)
	return withProjectLock(lockDir, func() error {
		stage, err := os.MkdirTemp(lockDir, "return-")
		if err != nil {
			return errors.New("could not prepare result return")
		}
		defer os.RemoveAll(stage)
		gitDir, index := filepath.Join(stage, "git"), filepath.Join(stage, "index")
		cmd := exec.Command("git", "init", "--bare", "--quiet", "--template=", gitDir)
		cmd.Env = patchEnvironment(index)
		if err := cmd.Run(); err != nil {
			return errors.New("could not prepare result index")
		}
		if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0o700); err != nil {
			return err
		}
		if err := configurePatchAttributes(gitDir); err != nil {
			return errors.New("could not configure result index")
		}
		if _, err := gitWithIndex(index, gitDir, captured.Tree, "add", "--force", "--", ".", ":(exclude)"+manifestName); err != nil {
			return errors.New("could not stage captured input")
		}
		base, err := gitWithIndex(index, gitDir, captured.Tree, "write-tree")
		if err != nil {
			return err
		}
		baseline := strings.TrimSpace(string(base))
		if err := applyGitPatch(index, gitDir, captured.Tree, patch, "--cached"); err != nil {
			return errors.New("worker patch does not match captured input")
		}
		// Disable rename detection so each entry has exactly one NUL-delimited
		// path. Spaces, tabs and quoted names are never parsed as shell syntax.
		raw, err := gitWithIndex(index, gitDir, captured.Tree, "diff", "--cached", "--raw", "-z", "--no-renames", "--no-ext-diff", baseline, "--")
		if err != nil {
			return err
		}
		parts := bytes.Split(raw, []byte{0})
		if len(parts) < 3 || len(parts)%2 != 1 || len(parts[len(parts)-1]) != 0 {
			return errors.New("worker patch has no supported changes")
		}
		original := make(map[string]fileEntry, len(captured.Manifest.Files))
		for _, entry := range captured.Manifest.Files {
			original[entry.Path] = entry
		}
		for i := 0; i < len(parts)-1; i += 2 {
			fields := strings.Fields(string(parts[i]))
			path := string(parts[i+1])
			if len(fields) != 5 || fields[1] != "000000" && fields[1] != "100644" && fields[1] != "100755" || safeRel(path) != nil || excludedPath(path) || validateOutputInputs(outputs, []fileEntry{{Path: path}}) != nil {
				return errors.New("worker patch changes an unsupported path or file type")
			}
			if err := checkReturnPath(root, path); err != nil {
				return errResultConflict
			}
			if entry, exists := original[path]; exists {
				hash, info, err := digestFile(root, path)
				if err != nil || hash != entry.Hash || info.Size() != entry.Size || normalizedMode(info.Mode()) != entry.Mode {
					return errResultConflict
				}
			} else if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(path))); !os.IsNotExist(err) {
				return errResultConflict
			}
		}
		if _, err := gitWithIndex(index, gitDir, root, "read-tree", baseline); err != nil {
			return err
		}
		// The index was built against the captured tree. Refresh its stat cache
		// against the live tree without changing its blobs. Unrelated dirty files
		// may make refresh report failure; apply checks only the affected paths.
		_, _ = gitWithIndex(index, gitDir, root, "update-index", "-q", "--refresh")
		if err := applyGitPatch(index, gitDir, root, patch, "--index", "--check"); err != nil {
			return errResultConflict
		}
		// --index checks the live preimages again before writing. This index
		// belongs to Switchyard, never to the caller's Git/Jujutsu repository.
		if err := applyGitPatch(index, gitDir, root, patch, "--index"); err != nil {
			return errors.New("could not return worker edits; inspect the retained patch")
		}
		return nil
	})
}

func workspaceLockDir(home, root string) string {
	digest := sha256.Sum256([]byte(root))
	return filepath.Join(home, "workspace-locks", hex.EncodeToString(digest[:]))
}

func checkReturnPath(root, path string) error {
	current := root
	components := append([]string{""}, strings.Split(path, "/")...)
	for i, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) && i > 0 {
			return nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || i < len(components)-1 && !info.IsDir() {
			return errResultConflict
		}
	}
	return nil
}

func recordEditReturn(home, job, state string) error {
	record, err := loadLocalJob(home, job)
	if err != nil {
		return err
	}
	record.EditReturn = state
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(home, "runs", job+".json"), append(content, '\n'))
}

func receiveEdits(ctx context.Context, provider shellProvider, home, job, root string, captured capture, status jobRecord, attempt *jobAttempt, autoApply, proxy bool, outputs []string) {
	attempt.phase = "patch"
	saveReturn := func(state string) {
		if err := recordEditReturn(home, job, state); err != nil {
			logEvent(home, "edit-return", "record-failed", time.Time{})
		}
	}
	if status.HasEdits {
		var patch []byte
		err := attempt.retry(ctx, "patch", func(ctx context.Context) error {
			var patchErr error
			_, patch, patchErr = provider.Call(ctx, []string{"patch", "--project", status.Project, "--job", job}, false)
			return patchErr
		})
		if err != nil || len(patch) == 0 || len(patch) > maxPatchSize {
			logEvent(home, "patch", "retrieve-failed", time.Time{})
			saveReturn("failed")
			if !attempt.automatic {
				fmt.Fprintln(os.Stderr, "switchyard: could not retrieve worker edits; preserving command exit status")
			}
			return
		}
		path := filepath.Join(home, "runs", job+".patch")
		if err := writeAtomic(path, patch); err != nil {
			logEvent(home, "patch", "save-failed", time.Time{})
			saveReturn("failed")
			if !attempt.automatic {
				fmt.Fprintln(os.Stderr, "switchyard: could not save worker edits; preserving command exit status")
			}
			return
		}
		state := "review"
		if autoApply {
			// Record the in-progress state before changing the caller's files.
			// An interrupted publication must not look like a successful return.
			err := recordEditReturn(home, job, "applying")
			if err == nil {
				err = returnWorkerEdits(home, root, captured, patch, outputs...)
			}
			state = "applied"
			if err != nil {
				state = "failed"
				if errors.Is(err, errResultConflict) {
					state = "conflict"
				}
				fmt.Fprintf(os.Stderr, "switchyard: edit return %s; review %s; preserving command exit status\n", state, path)
			}
		}
		saveReturn(state)
		logEvent(home, "edit-return", state, time.Time{})
		if !proxy {
			fmt.Printf("Worker edits: %s (%s)\n", path, state)
		}
	} else if status.EditsError == "" {
		saveReturn("none")
	}
	if status.EditsError != "" {
		logEvent(home, "patch", "collection-failed", time.Time{})
		saveReturn("failed")
	}
}
