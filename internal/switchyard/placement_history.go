package switchyard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	maxObservationBytes  = 512 << 10
	maxObservedCommands  = 512
	maxSamplesPerBackend = 5
)

type commandObservation struct {
	WallMS int64 `json:"wall_ms"`
	CPUMS  int64 `json:"cpu_ms,omitempty"`
}

type commandHistory struct {
	LastSeen int64                           `json:"last_seen"`
	Backends map[string][]commandObservation `json:"backends"`
}

func commandSignature(cwd, executable string, args []string) (string, bool) {
	if !filepath.IsAbs(cwd) || !filepath.IsAbs(executable) || len(args) > 256 {
		return "", false
	}
	if strings.ContainsAny(cwd+executable, "\x00\n") {
		return "", false
	}
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return "", false
		}
	}
	input, err := json.Marshal([]any{cwd, executable, append([]string{}, args...)})
	if err != nil || len(input) > 64<<10 {
		return "", false
	}
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:]), true
}

func observationFile(home string) string {
	return filepath.Join(home, "observations", "timings.json")
}

func observedBackendID(provider shellProvider) string {
	digest := sha256.Sum256([]byte(provider.Name() + "\x00" + endpointID(provider.cfg) + "\x00" + provider.cfg.Command + "\x00" + provider.cfg.Identity))
	return "p" + hex.EncodeToString(digest[:15])
}

func readObservations(home string) (map[string]commandHistory, error) {
	path := observationFile(home)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return make(map[string]commandHistory), nil
	} else if err != nil {
		return nil, errors.New("could not inspect command observations")
	}
	content, err := privateRead(path, maxObservationBytes)
	if err != nil {
		return nil, err
	}
	var history map[string]commandHistory
	if err := json.Unmarshal(content, &history); err != nil || history == nil || len(history) > maxObservedCommands {
		return nil, errors.New("command observation history is invalid")
	}
	for key, command := range history {
		if !snapshotPattern.MatchString(key) || len(command.Backends) > 32 {
			return nil, errors.New("command observation history is invalid")
		}
		for backend, samples := range command.Backends {
			if !remoteName.MatchString(backend) || len(samples) > maxSamplesPerBackend {
				return nil, errors.New("command observation history is invalid")
			}
			for _, sample := range samples {
				if sample.WallMS < 1 || sample.WallMS > (24*time.Hour).Milliseconds() || sample.CPUMS < 0 || sample.CPUMS > sample.WallMS*256 {
					return nil, errors.New("command observation history is invalid")
				}
			}
		}
	}
	return history, nil
}

func recordCommandObservation(home, signature, backend string, wall, cpu time.Duration) error {
	if !snapshotPattern.MatchString(signature) || !remoteName.MatchString(backend) || wall <= 0 || wall > 24*time.Hour || cpu < 0 {
		return errors.New("invalid command observation")
	}
	sample := commandObservation{WallMS: max(1, wall.Milliseconds()), CPUMS: cpu.Milliseconds()}
	// Validate the serialized units too: truncating wall time to milliseconds
	// can turn a valid duration pair into an unreadable history entry.
	if sample.CPUMS > sample.WallMS*256 {
		return errors.New("invalid command observation")
	}
	directory := filepath.Join(home, "observations")
	if err := privateTree(directory); err != nil {
		return err
	}
	fd, err := syscall.Open(filepath.Join(directory, ".lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return errors.New("could not lock command observations")
	}
	lock := os.NewFile(uintptr(fd), ".lock")
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return errors.New("command observation lock is not private")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return errors.New("could not lock command observations")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	history, err := readObservations(home)
	if err != nil {
		return err
	}
	command := history[signature]
	if command.Backends == nil {
		command.Backends = make(map[string][]commandObservation)
	}
	samples := append(command.Backends[backend], sample)
	if len(samples) > maxSamplesPerBackend {
		samples = samples[len(samples)-maxSamplesPerBackend:]
	}
	command.Backends[backend] = samples
	command.LastSeen = time.Now().Unix()
	history[signature] = command
	if len(history) > maxObservedCommands {
		oldest := signature
		for key, entry := range history {
			if key != signature && (oldest == signature || entry.LastSeen < history[oldest].LastSeen) {
				oldest = key
			}
		}
		delete(history, oldest)
	}
	content, err := json.Marshal(history)
	if err != nil || len(content) > maxObservationBytes {
		return errors.New("command observation history is too large")
	}
	return writeAtomic(observationFile(home), append(content, '\n'))
}

func medianWall(samples []commandObservation) int64 {
	if len(samples) < 2 {
		return 0
	}
	walls := make([]int64, len(samples))
	for index, sample := range samples {
		walls[index] = sample.WallMS
	}
	sort.Slice(walls, func(i, j int) bool { return walls[i] < walls[j] })
	return (walls[(len(walls)-1)/2] + walls[len(walls)/2]) / 2
}

func preferredObservedBackend(home, signature string, available []string) string {
	if !snapshotPattern.MatchString(signature) {
		return ""
	}
	history, err := readObservations(home)
	if err != nil {
		return ""
	}
	command := history[signature]
	local := medianWall(command.Backends["local"])
	if local == 0 {
		return ""
	}
	best, bestTime := "", local
	for _, name := range available {
		if name == "local" {
			continue
		}
		elapsed := medianWall(command.Backends[name])
		if elapsed > 0 && elapsed <= local*8/10 && elapsed < bestTime {
			best, bestTime = name, elapsed
		}
	}
	return best
}

// writeStatistics shows only hashed command identities and backend names. Each
// backend retains at most five samples per command, not a lifetime total.
func writeStatistics(home string, configured []shellProvider, output io.Writer, limit int) error {
	history, err := readObservations(home)
	if err != nil {
		return err
	}
	known := map[string]string{"local": "local"}
	for _, provider := range configured {
		known[observedBackendID(provider)] = provider.Name()
	}
	type row struct {
		signature string
		seen      int64
	}
	rows := make([]row, 0, len(history))
	for signature, entry := range history {
		rows = append(rows, row{signature, entry.LastSeen})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].seen == rows[j].seen {
			return rows[i].signature < rows[j].signature
		}
		return rows[i].seen > rows[j].seen
	})
	if _, err := fmt.Fprintf(output, "Commands observed: %d (at most %d recent samples per backend)\n", len(rows), maxSamplesPerBackend); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if _, err := fmt.Fprintln(output, "Command hash  Backend             Samples  Median wall (ms)"); err != nil {
		return err
	}
	for _, item := range rows {
		entry := history[item.signature]
		backends := make([]string, 0, len(entry.Backends))
		for backend := range entry.Backends {
			backends = append(backends, backend)
		}
		sort.Slice(backends, func(i, j int) bool {
			return known[backends[i]]+backends[i] < known[backends[j]]+backends[j]
		})
		for _, backend := range backends {
			label := known[backend]
			if label == "" {
				label = backend[:min(len(backend), 13)]
			}
			samples := entry.Backends[backend]
			median := medianWall(samples)
			if len(samples) == 1 {
				median = samples[0].WallMS
			}
			if _, err := fmt.Fprintf(output, "%-13s %-19s %7d %17d\n", item.signature[:12], label, len(samples), median); err != nil {
				return err
			}
		}
	}
	return nil
}
