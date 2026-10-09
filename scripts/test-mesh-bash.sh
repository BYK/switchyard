#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || $1 != /* || $2 != /* || ! -x $1 || ! -x $2 ]]; then
  printf 'Usage: %s /absolute/path/to/patched/bash /absolute/path/to/mesh-go\n' "$0" >&2
  exit 2
fi

bash_bin=$1
mesh_go=$2
root=$(mktemp -d -p /tmp/opencode mesh-bash-test.XXXXXXXX)
trap 'rm -rf -- "$root"' EXIT
mkdir -m 700 "$root/repo" "$root/bin"
printf '.env\n' > "$root/repo/.gitignore"
printf '{"name":"mesh-bash-test","private":true}\n' > "$root/repo/package.json"
printf "lockfileVersion: '9.0'\nimporters:\n  .: {}\n" > "$root/repo/pnpm-lock.yaml"
printf 'test source\n' > "$root/repo/README.md"
git -C "$root/repo" init -q
git -C "$root/repo" add .gitignore package.json pnpm-lock.yaml README.md
git -C "$root/repo" -c user.name=MeshFixture -c user.email=fixture@invalid.local commit -qm baseline
printf 'untracked work\n' > "$root/repo/new.txt"
printf 'synthetic excluded value\n' > "$root/repo/.env"
printf '%s\n' '#!/bin/sh' 'case "$1" in' \
  'install) exit 0 ;;' \
  'test) if [ "${MESH_SHELL_CAPTURE-}" != "invoked after launch" ]; then exit 86; fi; printf "cwd=%s\n" "$PWD"; printf "worker edit\n" > agent-note.txt; exit 37 ;;' \
  '*) exit 19 ;;' 'esac' > "$root/bin/pnpm"
printf '%s\n' '#!/bin/sh' 'exit 44' > "$root/bin/alternate-pnpm"
chmod 0755 "$root/bin/pnpm" "$root/bin/alternate-pnpm"

export MESH_GO_HOME="$root/state" MESH_BASH_PROXY="$mesh_go" MESH_BACKEND=probe
export PATH="$root/bin:$PATH"
"$mesh_go" worker --root "$root/worker" init
"$mesh_go" backend add --name probe --kind local --root "$root/worker" --priority 10 > /dev/null
preview_snapshot() {
  local actual expected
  actual=$("$mesh_go" preview --repo "$root/repo")
  expected=$'Snapshot: 5 files; 1 private paths excluded\n  + .gitignore\n  + README.md\n  + new.txt\n  + package.json\n  + pnpm-lock.yaml\n  - .env (excluded)'
  if [[ $actual != "$expected" ]]; then
    printf 'The disposable worktree selection changed:\n%s\n' "$actual" >&2
    exit 1
  fi
  printf '%s\n' "$actual" >&2
}

printf -v launch '%q --noprofile --norc -i' "$bash_bin"
printf 'mesh-test: interactive\n' >&2
preview_snapshot
status=0
(
  cd "$root/repo"
  printf 'export MESH_SHELL_CAPTURE="invoked after launch"\ncd . && pnpm test\nexit\n' | script -q -e -c "$launch" /dev/null
) > "$root/session.log" || status=$?
if [[ $status -ne 37 || -e $root/repo/agent-note.txt ]]; then
  printf 'Interactive Bash did not preserve the remote exit and source isolation\n' >&2
  exit 1
fi
session=$(< "$root/session.log")
if [[ $session != *"cwd=$root/worker/"* || $session != *'1 private paths excluded'* ]]; then
  printf 'Interactive Bash did not execute against the selected worker tree\n' >&2
  exit 1
fi
shopt -s nullglob
patches=("$root"/state/runs/*.patch)
if [[ ${#patches[@]} -ne 1 ]]; then
  printf 'The worker edit did not return as one patch\n' >&2
  exit 1
fi
patch=$(< "${patches[0]}")
if [[ $patch != *'diff --git a/agent-note.txt b/agent-note.txt'* ]]; then
  printf 'The returned patch is missing the worker edit\n' >&2
  exit 1
fi

status=0
printf 'mesh-test: temporary environment\n' >&2
preview_snapshot
(
  cd "$root/repo"
  "$bash_bin" --noprofile --norc -c 'MESH_SHELL_CAPTURE="invoked after launch" pnpm test'
) > "$root/temporary.log" 2>&1 || status=$?
if [[ $status -ne 37 || -e $root/repo/agent-note.txt ]]; then
  printf 'Bash temporary environment assignments did not route to an isolated worker\n' >&2
  exit 1
fi

status=0
printf 'mesh-test: function and substitution fallbacks\n' >&2
"$bash_bin" --noprofile --norc -c 'pnpm() { return 29; }; pnpm test' || status=$?
if [[ $status -ne 29 ]]; then
  printf 'A Bash function was intercepted as an external command\n' >&2
  exit 1
fi
(
  cd "$root/repo"
  "$bash_bin" --noprofile --norc -c 'MESH_SHELL_CAPTURE="invoked after launch"; export MESH_SHELL_CAPTURE; value=$(pnpm test); [[ $value == *"cwd=$PWD"* && $value != *"Snapshot:"* ]]'
)
if [[ ! -e $root/repo/agent-note.txt ]]; then
  printf 'Command substitution did not stay local\n' >&2
  exit 1
fi
rm -- "$root/repo/agent-note.txt"

status=0
printf 'mesh-test: resolved-path mismatch\n' >&2
preview_snapshot
(
  cd "$root/repo"
  "$bash_bin" --noprofile --norc -c "hash -p '$root/bin/alternate-pnpm' pnpm; pnpm test"
) > "$root/mismatch.log" 2>&1 || status=$?
mismatch=$(< "$root/mismatch.log")
if [[ $status -ne 2 || $mismatch != *'cannot offload'* ]]; then
  printf 'Bash command hashing changed the executed program\n' >&2
  exit 1
fi

printf 'Patched Bash: interactive routing, exit 37, patch isolation and local fallbacks passed\n'
