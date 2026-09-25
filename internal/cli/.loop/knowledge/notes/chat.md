<!-- loop area note · chat · updated 2026-09-24T16:37:38Z · coverage: ask.go chat.go chat_daemon.go digest.go · auto-maintained, verify against code -->
# Area: Chat — Conversational Commands (`internal/cli` chat surface)

## What this does
CLI commands for conversation: `loop ask` (single LLM smoke test), `loop chat` (multi-agent room REPL, local or daemon-hosted), and `loop digest` (headless knowledge-layer rebuild).

## Key files
- **ask.go** — `ask <prompt>`: one-shot provider call with streaming, flags for provider/model/base-url/api-key-env/system/retries. Intended as a provider smoke test.
- **chat.go** — `chat <room.yaml> [opening]`: local in-process room session. Wires usage meter, transcript, knowledge layer, pipeline runs (`roomRunSession`), deep dives, and the terminal REPL loop with slash-command dispatch.
- **chat_daemon.go** — daemon mode via `--daemon`/`--socket` (`$LOOP_DAEMON_SOCK` default socket): messages POSTed, replies via SSE transcript stream; catch-up prints last ~10 missed lines. Room outlives terminal — stdin close detaches, never kills.
- **digest.go** — `digest`: headless `roomkit.KnowledgeManager().Maintain()`; plus `maintainWorkspaceKnowledge` called after pipeline runs (incremental — unchanged workspace = zero model calls, silent).

## Connections
- Depends on `internal/llm` (providers, `resolveProvider`), `internal/chat` (Room engine owns slash verbs), `internal/roomkit` (room construction, knowledge layer), `internal/daemon` (client), `internal/config` (room YAML, `RoomsDir` = `.loop/rooms`), `internal/usage` (token meter), `internal/counters` (anonymous opt-in session counts).
- Knowledge layer lives in `.loop/knowledge/` (digest.md, notes/, index.json); refreshed by pipeline completions, reconciled at session open (`EnsureKnowledge`), rebuilt by `digest`.
- Pipeline runs inside chat re-exec the real `loop` binary; `executablePath` is a package var so tests can point it at a fresh build.

## Gotchas / invariants
- `--socket` without `--daemon` is an error.
- Reply/Say failures never end a session — printed as `✗` and the loop continues; same for a failed opening-message delivery.
- `/reset` and `/fork` are guarded: vetoed while a run (pipeline or deep dive) is alive or awaiting approval.
- Daemon `/quit`/`/exit`/`/detach` and stdin EOF all **detach** — never stop the hosted room.
- Bare `/` prints the command list (discoverability affordance).
- `handleRoomRunCommand` returns false for non-pipeline verbs so the room loop passes the line to agents; `/approve` auto-detects a leading pipeline alias only when the name is known and >1 word remain.
- Knowledge layer silently degrades: no provider → off with one notice (local) or silent return (post-run/digest path errors out only in the explicit `digest` command).
- Daemon deferred notes have a call cap — rerun `digest` to continue.