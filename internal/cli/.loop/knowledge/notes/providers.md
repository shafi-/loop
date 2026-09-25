<!-- loop area note · providers · updated 2026-09-25T12:21:39Z · coverage: arch_test.go cli_test.go provider.go provider_test.go roomrun_e2e_test.go roomrun_resolve_test.go run_human_test.go · auto-maintained, verify against code -->
# Providers & CLI Architecture Tests — Area Notes

## What this area does
Tests the `internal/cli` package: the composition root that wires surfaces (run, chat, ui) to core packages, plus the provider resolution entry point and the human-gate / room-run E2E flows.

## Key files
- `arch_test.go` — `TestCoreNeverImportsCLI` shells out to `go list` to pin layering: nothing under `internal/` imports `internal/cli` except cli itself; `webui` may import only the daemon client (never engine/runctl/cli).
- `cli_test.go` — `runCLI` helper executes real cobra commands with swapped `os.Stdin`. Covers `init` (scaffolds 7 files, seeds personas in `$HOME/.loop/personas`, skips-or-`--force`, legacy-dir hint), `validate` (strict decode), `run` pause/resume/failure, `chat` quit, `doctor` counters state, and `envPaths` replace-not-merge semantics.
- `provider.go` — `resolveProvider`: flags → `config.ModelConfig` → `llm.Build` (shared precedence flags > env > defaults; retry policy). Returns provider, resolved name, resolved model.
- `provider_test.go` — pins resolution: openai/anthropic env inference, explicit flag beats env, custom `--base-url` is keyless, unconfigured errors must name the env var that fixes it.
- `roomrun_e2e_test.go` — real-binary E2E via `loopBinary` (builds `./cmd/loop` once, package-level temp dir, skipped under `-short`). `roomSession` drives `chat` over os.Pipes with marker polling (`await`/`awaitCount`).
- `roomrun_resolve_test.go` — `resolveFile`: room alias wins, resolved relative to the room file's dir (`.loop/rooms/../pipelines/`); path fallback with label = base-minus-ext; `Runnable()` lists aliases then unaliased workspace pipelines.
- `run_human_test.go` — `classifyHumanLine` (answers vs `/pause` `/quit` `/exit` — exact lowercase match only) and `terminalHuman`: blank lines re-ask, `/pause` returns `engine.ErrPaused`.

## Connections
- Depends on `internal/llm` (Build), `internal/config` (ModelConfig, RoomPipeline), `internal/engine` (ErrPaused).
- `run` writes run state to `.loop/runs/<runID>/state.json` (`"paused"`, `"done"` fields); chat transcripts to `.loop/rooms/<name>/transcript.jsonl`.

## Gotchas / invariants
- Arch test shells out to `go list` — skipped under `-short`; honors `GO_BIN` env.
- E2E tests build the real binary; `executablePath` is stubbed via `t.Cleanup` restore. The built binary lives in a package-level `/tmp` dir (must outlive the first test), not `t.TempDir()`.
- CLI tests must isolate `$HOME` via `t.Setenv` — `init` seeds the global persona library and would touch the real `~/.loop`.
- Counters are opt-in: `LOOP_COUNTERS=1` + `LOOP_COUNTERS_FILE`; `envPaths` replaces