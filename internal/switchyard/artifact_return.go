package switchyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

var errArtifactConflict = errors.New("local outputs changed since dispatch; artifacts were not applied")

type stagedArtifact struct {
	target, stage string
	existed       bool
}

// Stage beside each destination so publication never crosses filesystems. Each
// rename exposes a complete root; replacing a nonempty directory has a brief
// missing-path window. Multiple roots are not one filesystem transaction.
// Keep backups on a publication failure; never retry or replay the command.
func publishArtifacts(home, root, extracted string, baseline artifactManifest) error {
	return withProjectLock(workspaceLockDir(home, root), func() error {
		matches := func() bool {
			current, err := inspectArtifacts(root, baseline.Outputs, true)
			return err == nil && reflect.DeepEqual(current, baseline)
		}
		if !matches() {
			return errArtifactConflict
		}
		var staged []stagedArtifact
		preserve := false
		defer func() {
			if !preserve {
				for _, item := range staged {
					os.RemoveAll(item.stage)
				}
			}
		}()
		for _, output := range baseline.Outputs {
			target := filepath.Join(root, filepath.FromSlash(output))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return errors.New("could not prepare output parent")
			}
			stage, err := os.MkdirTemp(filepath.Dir(target), ".switchyard-return-")
			if err != nil {
				return errors.New("could not stage artifact publication")
			}
			_, statErr := os.Lstat(target)
			staged = append(staged, stagedArtifact{target: target, stage: stage, existed: statErr == nil})
			source := filepath.Join(extracted, filepath.FromSlash(output))
			if err := copyTree(source, filepath.Join(stage, "new")); err != nil {
				return errors.New("could not stage complete output")
			}
			// copyTree keeps scratch directories private. Set the declared
			// directory mode while they are still inside the private stage.
			if err := filepath.WalkDir(filepath.Join(stage, "new"), func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return os.Chmod(path, 0o755)
				}
				return nil
			}); err != nil {
				return errors.New("could not set output directory permissions")
			}
		}
		// Copying large bundles takes time. Recheck all roots before any replace.
		if !matches() {
			return errArtifactConflict
		}
		for _, item := range staged {
			preserve = true
			if item.existed {
				if err := os.Rename(item.target, filepath.Join(item.stage, "previous")); err != nil {
					return errors.New("could not back up output before replacement")
				}
			}
			if err := os.Rename(filepath.Join(item.stage, "new"), item.target); err != nil {
				if item.existed {
					_ = os.Rename(filepath.Join(item.stage, "previous"), item.target)
				}
				return errors.New("could not publish output; inspect retained staging directories")
			}
		}
		preserve = false
		return nil
	})
}

func recordArtifactReturn(home, job, state string) error {
	record, err := loadLocalJob(home, job)
	if err != nil {
		return err
	}
	record.ArtifactReturn = state
	content, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(home, "runs", job+".json"), append(content, '\n'))
}

func receiveArtifacts(ctx context.Context, provider shellProvider, home, job, root string, baseline artifactManifest, status jobRecord, attempt *jobAttempt, proxy bool) {
	if len(baseline.Outputs) == 0 {
		return
	}
	state := "failed"
	path := filepath.Join(home, "runs", job+".artifacts.tar")
	defer func() {
		if err := recordArtifactReturn(home, job, state); err != nil {
			logEvent(home, "artifact-return", "record-failed", time.Time{})
		}
		logEvent(home, "artifact-return", state, time.Time{})
		if state == "failed" || state == "conflict" {
			fmt.Fprintf(os.Stderr, "switchyard: artifact return %s for job %s; inspect status and retained results; preserving command exit status\n", state, job)
		} else if !proxy {
			fmt.Printf("Worker artifacts: %s (%s)\n", path, state)
		}
	}()
	if status.ArtifactError != "" || !equalPaths(status.Outputs, baseline.Outputs) || requireSnapshot(status.ArtifactHash) != nil || status.ArtifactSize <= 0 || status.ArtifactSize > maxArtifactBundle {
		return
	}
	var data []byte
	err := attempt.retry(ctx, "artifacts", func(ctx context.Context) error {
		var err error
		_, data, err = provider.Call(ctx, []string{"artifacts", "--project", status.Project, "--job", job}, false)
		return err
	})
	if err != nil || int64(len(data)) != status.ArtifactSize {
		return
	}
	// Unpack into private scratch storage. Nothing under the live output roots
	// is touched until every member and the complete transport digest validates.
	extracted, err := os.MkdirTemp(filepath.Join(home, "runs"), "artifact-")
	if err != nil {
		return
	}
	defer os.RemoveAll(extracted)
	if err := unpackArtifacts(data, status.ArtifactHash, baseline.Outputs, extracted); err != nil {
		return
	}
	if err := writeAtomic(path, data); err != nil {
		return
	}
	apply, err := applyResults(true) // Declaring outputs opts into their return, even for explicit run.
	if err != nil {
		return
	}
	if !apply {
		state = "review"
		return
	}
	if err := recordArtifactReturn(home, job, "applying"); err != nil {
		return
	}
	if err := publishArtifacts(home, root, extracted, baseline); err != nil {
		if errors.Is(err, errArtifactConflict) {
			state = "conflict"
		}
		return
	}
	state = "applied"
}
