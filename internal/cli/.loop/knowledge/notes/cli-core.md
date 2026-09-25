<!-- loop area note · cli-core · updated 2026-09-24T18:26:08Z · coverage: init.go new.go roomrun.go root.go · auto-maintained, verify against code -->
# Area: CLI Core (internal/cli)

## What it does
Cobra command surface for `loop`. `root.go` wires all subcommands, loads dotenv config, and maps SIGINT/SIGTERM to context cancellation. `init.go` scaffolds a `.loop/` workspace (pipelines, rooms, README, global personas). `new.go` generates a pipeline YAML from a plain-language description via LLM with validator-driven repair. `roomrun.go` is the bridge between chat rooms and pipeline runs: a room spawns real `loop run` subprocesses through the shared supervisor.

## Key files
- `root.go` — `NewRootCmd()` (all subcommands registered here), `Execute()` with signal-aware context, dotenv loading (`envPaths`/`loadDotenv`). `version` set via `-ldflags`.
- `init.go` — `loop init [dir] [--force]`: writes example pipelines/rooms/README, seeds global personas (`seedGlobalPersonas`, defined elsewhere), warns about legacy root-level dirs, bumps counter.
- `new.go` — `loop new <description>`: resolves provider (via `resolveProvider`, defined elsewhere in package), runs `generator.Generator` with validate-repair loop; writes YAML to `-o` (file or dir) or stdout; `--max-repairs` (default 2).
- `roomrun.go` — `roomRunSession`: wraps `runctl.Supervisor` per room; alias→file resolution, gate/approval rendering, transcript audit lines, deep-dive coordination.

## Connections
- `config` (PipelinesDir, RoomsDir, LoopDir, ParsePipeline), `generator`, `runctl`, `chat`, `roomkit`, `counters`, `dotenv`, `examples` packages. Other commands (`setup`, `validate`, `run`, `chat`, etc.) are defined in sibling files not covered here.

## Invariants / gotchas
- All artifacts live under `.loop/`; legacy root-level `rooms/pipelines/personas/plans` are invisible — `init` warns with a `mv` hint but never migrates.
- Signal handling: first SIGINT/SIGTERM cancels ctx (run records resume point, exits non-zero); second signal restores default handler and force-kills.
- dotenv: explicit `--env-file` paths **replace** the default; otherwise `.env` then `$LOOP_ENV_FILE`; real environment always wins over file values; missing files are not errors.
- Room-relative pipeline paths: alias files resolve relative to the room file's directory (`filepath.Join(filepath.Dir(s.roomPath), p.File)`); absolute paths pass through.
- `resolveFile` falls back to treating the arg as an existing `.yaml`/`.yml` path; run label = base name minus extension.
- Room auto-injects `--var room_context=<handoff path>` and `--var room=<name>` unless the user explicitly set `room_context` themselves (see `hasVarArg`). These two keys are excluded from unknown-var warnings (`userVarKeys`).
- `warnUnknownVars` only warns (never errors) on `--var` keys not declared in the pipeline; parse failures of the pipeline are silently ignored here — the child run reports them.
- `runsBusy` vetoes room reset/fork while any supervised run is alive or waiting.
- Dive support is