package switchyard

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

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
