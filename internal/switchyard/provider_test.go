package switchyard

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestManagedWorkerObservationsSurviveAddressChange(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "aws-worker")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := awsKeyPath(dir)
	if err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).Run(); err != nil {
		t.Fatal(err)
	}
	public, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(awsKnownPath(dir), append([]byte(awsName+" "), public...), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := backendConfig{Name: "aws", Kind: "ssh", Root: awsRoot, Command: awsCommand,
		Host: "192.0.2.1", User: "ubuntu", Identity: key, KnownHosts: awsKnownPath(dir),
		HostKeyAlias: awsName, Capabilities: []string{"cpu"}}
	if err := saveBackend(home, backend); err != nil {
		t.Fatal(err)
	}
	legacy := observedBackendID(shellProvider{cfg: backend})
	backend.Host = "192.0.2.2"
	if err := upsertAWSBackend(home, backend); err != nil {
		t.Fatal(err)
	}
	entries, err := loadBackends(home)
	if err != nil || len(entries) != 1 || observedBackendID(shellProvider{cfg: entries[0]}) != legacy {
		t.Fatalf("restarting a pinned worker lost its measured history: %v", err)
	}
	backend.Host = "192.0.2.3"
	if err := upsertAWSBackend(home, backend); err != nil {
		t.Fatal(err)
	}
	entries, err = loadBackends(home)
	if err != nil || observedBackendID(shellProvider{cfg: entries[0]}) != legacy {
		t.Fatalf("another address change lost its measured history: %v", err)
	}
}

func TestDestroyedAWSBackendDoesNotStrandOtherBackends(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "aws-worker")
	local := backendConfig{Name: "sandbox", Kind: "local", Root: t.TempDir()}
	managed := backendConfig{Name: "aws", Kind: "ssh", Root: awsRoot, Command: awsCommand,
		Identity: awsKeyPath(dir), KnownHosts: awsKnownPath(dir), HostKeyAlias: awsName}
	content, err := json.Marshal(backendFile{Backends: []backendConfig{managed, local}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(configFile(home), content); err != nil {
		t.Fatal(err)
	}
	if _, err := loadBackends(home); err == nil {
		t.Fatal("missing worker credentials did not invalidate the old backend")
	}
	if err := removeDestroyedAWSBackend(home, dir); err != nil {
		t.Fatal(err)
	}
	entries, err := loadBackends(home)
	if err != nil || len(entries) != 1 || entries[0].Name != "sandbox" {
		t.Fatalf("destroy left a stale backend or removed user configuration: %v %v", err, entries)
	}
	if err := removeDestroyedAWSBackend(home, dir); err != nil {
		t.Fatalf("repeat cleanup failed: %v", err)
	}
	managed.Root = "/another/worker"
	content, err = json.Marshal(backendFile{Backends: []backendConfig{managed, local}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(configFile(home), content); err != nil {
		t.Fatal(err)
	}
	if err := removeDestroyedAWSBackend(home, dir); err == nil {
		t.Fatal("destroy removed an unrelated backend named aws")
	}
}
