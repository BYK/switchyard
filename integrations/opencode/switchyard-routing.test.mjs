import assert from "node:assert/strict"
import { test } from "node:test"
import os from "node:os"
import path from "node:path"
import { SwitchyardRoutingPlugin } from "./switchyard-routing.js"

const hook = (await SwitchyardRoutingPlugin({ directory: "/tmp/opencode/fixture" }))["tool.execute.before"]
const binary = `'${path.join(os.homedir(), ".local", "bin", "switchyard").replaceAll("'", "'\"'\"'")}'`

test("ordinary build commands enter automatic placement without a marker", async () => {
  for (const tool of ["bash", "functions.bash"]) {
    const args = { command: "pnpm test", workdir: "/tmp/opencode/fixture" }
    await hook({ tool }, { args })
    assert.match(args.command, /switchyard.* shell --repo '\/tmp\/opencode\/fixture' -- -c 'pnpm test'$/)
    assert.doesNotMatch(args.command, /TRACE_PREVIEWED/)
  }
})

test("ordinary shell syntax and unrelated calls keep their original behavior", async () => {
  for (const [tool, command] of [["read", "pnpm test"], ["bash", "git status"], ["bash", "pnpm dev"],
    ["bash", "pnpm test --watch"], ["bash", "make --version"],
    ["bash", "pnpm test && true"], ["bash", "pnpm test > output.txt"], ["bash", "pnpm test | cat"]]) {
    const args = { command, workdir: "/tmp/opencode/fixture" }
    await hook({ tool }, { args })
    assert.equal(args.command, command)
  }
})

test("explicit previewed commands retain their requested backend", async () => {
  const args = { command: "SWITCHYARD_TRACE_PREVIEWED=1 SWITCHYARD_BACKEND=aws pnpm test", workdir: "/tmp/opencode/fixture" }
  await hook({ tool: "bash" }, { args })
  assert.equal(args.command, `exec ${binary} run --backend aws --repo '/tmp/opencode/fixture' -- pnpm test`)
})

test("forced shell expressions fail instead of silently running locally", async () => {
  const args = { command: "SWITCHYARD_TRACE_PREVIEWED=1 SWITCHYARD_BACKEND=aws pnpm test && true", workdir: "/tmp/opencode/fixture" }
  await assert.rejects(hook({ tool: "bash" }, { args }), /literal build or test command/)
})

test("worktree paths are quoted and relative paths are rejected", async () => {
  const args = { command: "go test ./...", workdir: "/tmp/opencode/it's a fixture" }
  await hook({ tool: "bash" }, { args })
  assert.match(args.command, /--repo '\/tmp\/opencode\/it'"'"'s a fixture'/)
  await assert.rejects(hook({ tool: "bash" }, { args: { command: "pnpm test", workdir: "relative" } }), /absolute worktree/)
})

test("workspace selectors and recursive build flags route automatically", async () => {
  for (const command of ["pnpm --filter @sentry/cli test", "pnpm -r run typecheck", "npm --workspace cli run build"]) {
    const args = { command, workdir: "/tmp/opencode/fixture" }
    await hook({ tool: "bash" }, { args })
    assert.match(args.command, / shell --repo /)
  }
})

test("Bash startup files keep the original command untouched", async (t) => {
  const before = process.env.BASH_ENV
  t.after(() => { if (before === undefined) delete process.env.BASH_ENV; else process.env.BASH_ENV = before })
  process.env.BASH_ENV = "/tmp/opencode/startup-fixture"
  const args = { command: "pnpm test", workdir: "/tmp/opencode/fixture" }
  await hook({ tool: "bash" }, { args })
  assert.equal(args.command, "pnpm test")
})
