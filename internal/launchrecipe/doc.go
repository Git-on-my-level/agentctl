// Package launchrecipe deterministically builds bounded native launch argv for
// structured delegation. It emits exact flag vectors and prompt-delivery hints;
// prompt bytes stay outside the recipe.
//
// Guarantees (when Build succeeds):
//   - Argv is shell-free: each element is one argument; no re-parsing.
//   - Structured-output flags required by agentctl adapter manifests are present.
//   - Explicit speed and effort constraints are reflected in argv or rejected.
//   - Permission-broadening flags are injected only when
//     UnattendedCodingPermissions is true, access is coding (the default),
//     and the harness has a reviewed flag: Cursor --force, Codex
//     --dangerously-bypass-approvals-and-sandbox. Read-only access never
//     receives them. Other harnesses are labeled unsupported and are not
//     given an inferred bypass.
//   - Cursor --trust is included only when CursorWorkspaceTrust is true.
//     Trust does not imply --force, and --force does not imply trust.
//
// Non-guarantees:
//   - A configured model id is a requested tuple, not proof the worker will use
//     that model or provider at runtime.
//   - Effort is never inferred from model slug suffixes such as "-high".
//   - Claude Code effort/speed mapping is omitted unless explicitly supported.
package launchrecipe
