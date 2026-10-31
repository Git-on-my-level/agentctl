package launchrecipe

import "strings"

// ZCodeReadOnlyProfileEnv marks a launch whose native adapter must lay down
// the reviewed read-only project config before the child starts. Only this
// recipe sets it; expert `run` never does, so exact argv stays exact.
const ZCodeReadOnlyProfileEnv = "AGENTCTL_ZCODE_READ_ONLY_PROFILE=1"

// ZCodeReadOnlyDisallowedTools removes every built-in tool that can write,
// execute, spawn, message, or reach the network. Verified against ZCode CLI
// 0.17.0: with the read-only project config (which turns off plugins and so
// their MCP tools, including the JavaScript REPL), only Read, Glob, Grep,
// TodoRead, ReadSessionContext and AskUserQuestion remain. AskUserQuestion
// and ExitPlanMode fail headless ("No permission client configured").
var ZCodeReadOnlyDisallowedTools = []string{
	"Bash", "Edit", "Write", "Agent", "Skill", "ExitPlanMode", "EnterPlanMode",
	"TodoWrite", "TaskStop", "TaskOutput", "SendMessage", "ReplyToChannel",
	"WebFetch", "WebSearch", "mcp__web_reader__webReader",
}

// ZCode has no verified --model or --thinking flag. Headless --json uses the
// CLI's configured model and reasoningLevel. ZCODE_MODEL selects a provisioned
// per-model config through the host-managed launcher; unsupported tuples fail.
//
// read_only uses plan mode, which alone is not enforcement (Bash still runs in
// it), plus the explicit tool denylist and the adapter's read-only project
// config. The flag cannot remove plugin MCP tools; only the project config can.
func buildZCode(executable, model, speed, effort, access string) (Recipe, error) {
	exe, err := resolveExecutable("zcode", executable)
	if err != nil {
		return Recipe{}, err
	}
	if speed == "fast" {
		return Recipe{}, fail("speed_unavailable", "zcode cannot guarantee fast service speed")
	}
	nativeModel := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "zai/")
	if nativeModel != "glm-5.3" && nativeModel != "glm-5.3-flash" {
		return Recipe{}, fail("model_unavailable", "zcode launcher supports only zai/glm-5.3 and zai/glm-5.3-flash via provisioned ZCODE_MODEL configs")
	}
	// Effort is not a native flag: delegate checks it against the selected
	// profile at preflight. Static recipe compatibility cannot attest to it.
	// Clear ambient selectors so the base model uses the configured default,
	// including on hosts that have no provisioned per-model config.
	env := []string{"ZCODE_MODEL="}
	if nativeModel == "glm-5.3-flash" {
		env[0] += nativeModel
	}
	if access == AccessReadOnly {
		argv := []string{exe, "--json", "--mode", "plan", "--disallowed-tools=" + strings.Join(ZCodeReadOnlyDisallowedTools, ","), "--prompt"}
		return Recipe{Argv: argv, PromptDelivery: PromptDeliveryArgv, Env: append(env, ZCodeReadOnlyProfileEnv)}, nil
	}
	return Recipe{Argv: []string{exe, "--json", "--mode", "yolo", "--prompt"}, PromptDelivery: PromptDeliveryArgv, Env: env}, nil
}
