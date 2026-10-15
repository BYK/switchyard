import path from "node:path"
import os from "node:os"

const SWITCHYARD = path.join(os.homedir(), ".local", "bin", "switchyard")
const OPT_IN = /^SWITCHYARD_TRACE_PREVIEWED=1 SWITCHYARD_BACKEND=(aws(?:-[2-4])?|auto) (.+)$/
const LITERAL_COMMAND = /^(?:pnpm|npm|go|cargo|pytest|uv|make)(?: [a-zA-Z0-9_./@:+,=-]+)*$/
const HEAVY_COMMAND = /^(?:(?:pnpm|npm) (?:run )?(?:test|build|lint|check|typecheck)(?:[:.-][a-zA-Z0-9_.:-]+)?|(?:pnpm|npm) (?:exec )?vitest run|go (?:test|build|vet)|cargo (?:test|build|check|clippy|bench)|pytest|uv run pytest|make(?: (?:test|build|check|all))?)(?: [a-zA-Z0-9_./@:+,=-]+)*$/
const quote = (value) => `'${value.replaceAll("'", "'\"'\"'")}'`

export const SwitchyardRoutingPlugin = async ({ directory }) => ({
  "tool.execute.before": async (input, output) => {
    if ((input.tool !== "bash" && input.tool !== "functions.bash") || typeof output.args?.command !== "string") return
    const match = OPT_IN.exec(output.args.command)
    const command = match ? match[2] : output.args.command
    if (match && !LITERAL_COMMAND.test(command)) {
      throw new Error("Switchyard offload requires one literal build or test command; use switchyard run directly for other commands")
    }
    const normalized = command.replace(/^(pnpm|npm) (?:(?:--filter|-F|--workspace|--dir|-C) [a-zA-Z0-9_./@:+,=-]+ |(?:--recursive|-r|--workspace-root) )+/, "$1 ")
    if (!match && (!LITERAL_COMMAND.test(command) || !HEAVY_COMMAND.test(normalized) || /(?:^| )(?:--watch|--watchAll|--ui|--help|--version)(?: |$)/.test(command))) return
    // Rewriting must never run Bash startup files twice or bypass an exported
    // function that the user's original Bash command would have resolved.
    if (!match && (process.env.BASH_ENV || process.env[`BASH_FUNC_${command.split(" ")[0]}%%`])) return
    const repo = output.args.workdir ?? directory
    if (typeof repo !== "string" || !path.isAbsolute(repo) || /[\r\n\0]/.test(repo)) {
      throw new Error("Switchyard offload requires an absolute worktree directory")
    }
    // Native execution happens after OpenCode's tool permissions. Switchyard
    // owns pressure selection and fresh snapshot checks, never a global marker.
    output.args.command = match
      ? `exec ${quote(SWITCHYARD)} run --backend ${match[1]} --repo ${quote(repo)} -- ${command}`
      : `exec ${quote(SWITCHYARD)} shell --repo ${quote(repo)} -- -c ${quote(command)}`
  },
})
