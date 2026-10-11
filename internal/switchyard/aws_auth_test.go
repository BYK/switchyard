package switchyard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeAWSForAuth(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	// The fake never prints the parent's credential values. It rejects them.
	script := `#!/bin/sh
if [ "${AWS_PROFILE:-}" != "switchyard" ] || [ -n "${AWS_ACCESS_KEY_ID:-}" ] || [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] || [ -n "${AWS_SESSION_TOKEN:-}" ]; then
  exit 9
fi
if [ "${FAKE_AWS_ROLE:-}" = root ]; then
  printf '%s\n' '{"Account":"123456789012","Arn":"arn:aws:iam::123456789012:root"}'
else
  printf '%s\n' '{"Account":"123456789012","Arn":"arn:aws:sts::123456789012:assumed-role/SwitchyardWorker/synthetic"}'
fi
`
	if err := os.WriteFile(filepath.Join(bin, "aws"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "untrusted-test-value")
	return filepath.Join(t.TempDir(), "worker")
}

func TestAWSAuthPinsRoleAndIgnoresParentCredentials(t *testing.T) {
	dir := fakeAWSForAuth(t)
	t.Setenv("SWITCHYARD_AWS_HOME", dir)
	t.Setenv("AWS_PROFILE", "root")
	if code := runAWS(t.TempDir(), []string{"auth", "select", "switchyard"}); code != 0 {
		t.Fatalf("could not select named profile with an inherited root profile: %d", code)
	}
	t.Setenv("AWS_PROFILE", "")
	info, err := os.Stat(awsAuthPath(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("selected AWS profile is not private: %v", err)
	}
	account, err := awsAccount()
	if err != nil || account != "123456789012" {
		t.Fatalf("selected role did not authenticate: %v", err)
	}
	if err := awsAuthSelect(dir, "switchyard", "SwitchyardWorker"); err != nil {
		t.Fatalf("reselecting the same verified role should be idempotent: %v", err)
	}
	if code := runAWS(t.TempDir(), []string{"auth", "status"}); code != 0 {
		t.Fatalf("could not verify selected role through the CLI: %d", code)
	}
	if err := awsAuthSelect(dir, "another", "SwitchyardWorker"); err == nil {
		t.Fatal("replaced a selected profile without inspection")
	}
	t.Setenv("AWS_PROFILE", "root")
	if _, err := awsAccount(); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting AWS_PROFILE must fail closed: %v", err)
	}
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("FAKE_AWS_ROLE", "root")
	if _, err := awsAccount(); err == nil {
		t.Fatal("a selected profile that stopped assuming its role was still accepted")
	}
	t.Setenv("FAKE_AWS_ROLE", "")
	if err := os.WriteFile(awsAuthPath(dir), []byte(`{"profile":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := awsCall("sts", "get-caller-identity"); err == nil {
		t.Fatal("invalid selected profile fell back to the parent's AWS credentials")
	}
}

func TestAWSAuthRejectsRootAndWrongWorkerAccount(t *testing.T) {
	dir := fakeAWSForAuth(t)
	t.Setenv("SWITCHYARD_AWS_HOME", dir)
	t.Setenv("FAKE_AWS_ROLE", "root")
	if err := awsAuthSelect(dir, "switchyard", "SwitchyardWorker"); err == nil {
		t.Fatal("root identity accepted as the dedicated worker role")
	}
	t.Setenv("FAKE_AWS_ROLE", "")
	if err := privateTree(dir); err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(awsState{Account: "000000000000", Region: awsRegion})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(awsStatePath(dir), content); err != nil {
		t.Fatal(err)
	}
	if err := awsAuthSelect(dir, "switchyard", "SwitchyardWorker"); err == nil {
		t.Fatal("role from another account accepted for the tracked worker")
	}
	if _, err := os.Lstat(awsAuthPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("failed selection persisted a profile: %v", err)
	}
}
