<!-- loop project digest · updated 2026-09-25T12:21:53Z · auto-maintained, verify against code -->
# Project Digest — loop CLI

**What:** `loop` — a Go CLI for orchestrating multi-agent LLM work. Users define pipelines (agent stages in YAML) and chat rooms (multi-agent conversations); pipelines run via a supervised engine (foreground or daemon-hosted), and rooms can spawn pipeline runs, deep dives, and maintain a knowledge layer. Serves developers running agentic workflows from the terminal.

**Structure:** `cmd/loop` is the binary; `internal/cli` is the cobra composition root (`root.go` wires all subcommands), split across `init.go` (workspace scaffolding), `new.go` (LLM-generated pipelines with validate-repair), `chat`/`chat_daemon` (room REPL, local or daemon via SSE), `run`/`run_daemon` (pipeline execution), `roomrun` (rooms spawning `loop run` subprocesses via a shared supervisor), `executor`/`doctor`, `provider.go` (`resolveProvider` precedence: flags > env > defaults), `assets` (go:embed templates), and a test suite including an architecture test and real-binary E2E tests.

**Conventions:**
- All artifacts live under `.loop/` (`pipelines/`, `rooms/`, `knowledge/`, `runs/`); legacy root-level dirs are warned about, never migrated.
- Use `config.LoadPipeline` (never `ParsePipeline`) so persona library refs resolve.
- Never import `internal/cli` from `internal/` — enforced by `TestCoreNeverImportsCLI`.
- `assets.go` is the only reader of `assets/`; room.yaml template keys are de-facto public config API.
- Gate prompts → stderr; stdout stays clean. Reply failures never kill sessions; `/pause` returns `engine.ErrPaused` for resumable stops.
- Knowledge layer degrades silently without a provider; `maintainWorkspaceKnowledge` is incremental (zero calls if workspace unchanged).
- Test hygiene: isolate `$HOME` via `t.Setenv`; arch/E2E tests skip under `-short`; `executablePath` is a stubbed package var.

**Current state:** Stable command surface (`init`, `new`, `run`, `chat`, `ask`, `digest`, `validate`, `doctor`, `executor install`). Recent work centers on daemon-mode chat (SSE streaming, detach semantics), room-run coordination (approval gates, deep dives, run-busy veto of reset/fork), and the knowledge layer (`.loop/knowledge/` digest/notes/index, refreshed on pipeline completion and reconciled at session open).

**Area notes index:**
- assets — Embedded room.yaml/README templates; single access point `assets.go`.
- chat — `ask`, `chat`, `digest` commands; daemon mode and knowledge-layer upkeep.
- cli-core — Root wiring, `init`/`new