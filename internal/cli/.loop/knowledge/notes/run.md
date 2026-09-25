<!-- loop area note · run · updated 2026-09-24T16:38:32Z · coverage: executor.go run.go run_daemon.go · auto-maintained, verify against code -->
# Area: Run — command execution & daemon-based running

## What it does
CLI layer for `loop run`, `loop executor install`, and `loop doctor`. Wires a pipeline into the engine's `Runner` (foreground), or submits it to a `loop serve` daemon and renders its event stream locally (`--daemon`). Handles human gate prompts, pause/resume, narrator wiring, token metering, and counters.

## Key files
- **executor.go** — `executorRegistry()` (registers the cline executor); `loop executor install <name>` (only `cline`; Node 22+ + network, installs standalone host); `loop doctor` health check (env files, cline host, counters). `runDoctor` is shared with `loop setup`.
- **run.go** — `newRunCmd` (`loop run`), `terminalHuman` (gate prompts on stderr, reads stdin), `classifyHumanLine`, pause-command handling, foreground run flow.
- **run_daemon.go** — `runViaDaemon`: dial daemon socket, submit, stream events, POST gate answers, ctrl-c → best-effort `Halt`.

## Connections
- `config.LoadPipeline` (never `ParsePipeline` — see gotchas) resolves personas; `--var k=v` overrides `pipeline.Vars`.
- `engine.Runner` does the real work; this area only sets `Human`, `Narrator`, `Executors`, `Usage` meter, runs dir/id, and renders results.
- Narrator: opt-in via `pipeline.Runtime.Narrator`; its provider is wrapped with `usage.Wrap(n.Provider, meter, "narrator")` after construction. Narrator failures never affect the run.
- Counters (`internal/counters`, opt-in via `LOOP_COUNTERS=1`) count `pipeline_runs` vs `pipeline_reruns` at run start (attempt counted, not success).
- Daemon client/protocol: `internal/daemon` (`Dial`, `Submit`, `Stream`, `Answer`, `Run`, `Halt`).
- Post-completion: `maintainWorkspaceKnowledge` (knowledge/briefs area) updates workspace docs incrementally.
- `loop setup` reuses `runDoctor`.

## Gotchas / invariants
- **Use `config.LoadPipeline`, not `ParsePipeline`**: personas may be library refs (`persona: engineer`); unresolved refs fail every agent stage with "persona not found in pipeline".
- Run ID is resolved **before** any consumer (runner registration + start line must agree).
- Gate prompts write to **stderr**; stdout stays clean for stage output.
- Empty line at a human prompt re-asks (never counts as an answer); `/pause`, `/quit`, `/exit` return `engine.ErrPaused` — clean, resumable stop.
- Pauses vs failures: pause with `res.Err != nil` = interrupted (failure-style exit code but resumable); plain pause exits 0. Failures are never suppressed by `--quiet`.
- `--daemon` flag rules: `--run-id` rejected with `--daemon` (daemon assigns ids); `--socket` only valid with `--daemon`. Resume with `--daemon --resume <id>`.
- Daemon detach: child process + its stdin belong to the daemon; local client only renders the event stream and POSTs gate answers. Empty daemon