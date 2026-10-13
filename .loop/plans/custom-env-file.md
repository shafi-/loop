# Plan: custom env file path (`--env-file`)

## Problem
loop loads only `.env` (cwd) at startup. `$LOOP_ENV_FILE` exists but is a
chicken-and-egg override (the override itself must be an env var), has no CLI
flag, and `loop doctor` hardcodes a ".env found" message.

Verified current code:
- `internal/cli/root.go`: `Execute()` calls `loadDotenv()` before cobra runs;
  paths = `[".env"]` + `$LOOP_ENV_FILE`.
- `internal/dotenv/dotenv.go`: policy already correct — shell env wins, file
  fills gaps, missing files are no-ops, malformed lines warn.

## Design
- Persistent root flag `--env-file <path>`, repeatable (`StringArray`).
- Path list resolution:
  - `--env-file` given → load exactly those paths, in order. Explicit flag
    *replaces* the `.env` default (grand rule: explicit wins over convention).
  - no flag → current behavior: `.env`, then `$LOOP_ENV_FILE`.
- Load order semantics: left-to-right; each file only fills gaps; the real
  shell environment always wins. Pin this with a test.
- Move loading from `Execute()` into root `PersistentPreRun` so cobra has
  parsed flags first. Risk: child commands defining their own
  `PersistentPreRun` would shadow it — verify none do (grep
  `PersistentPreRun` across `internal/cli`); if one appears later, chain to
  root's loader explicitly.

## Files to touch
1. `internal/cli/root.go` — add flag, move `loadDotenv` to
   `PersistentPreRun`, accept multiple explicit paths, keep the
   "loaded N variables" notice for every successfully loaded path (not just
   `.env`).
2. `internal/cli/executor.go` — doctor message: report the actually-loaded
   env files instead of hardcoded ".env". Simplest: `dotenv` package gains a
   tiny `Loaded() []string` recorded by `Load` successes, or `loadDotenv`
   returns paths stored in a package var in cli.
3. `internal/dotenv/dotenv.go` — no behavior change.
4. Tests (`internal/cli`):
   - flag-only: custom path loaded, `.env` ignored.
   - repeated flags: left-to-right, first-seen key kept (shell > file1 > file2).
   - no flag: `.env` + `LOOP_ENV_FILE` fallback (regression).
   - missing custom path: warning, non-zero not required (match current
     missing-file tolerance).
5. Docs: README / USER MANUAL — one line: `--env-file` replaces `.env`.

## Trade-offs
- Repeated-flag ordering is the only subtle part; keep left-to-right + shell
  wins and pin with a test rather than adding merge modes.
- Doctor reporting via a package-level "loaded paths" var is slightly global,
  but it matches the existing one-shot load model and is test-friendly.

## Build order
root.go flag + PreRun → doctor fix → tests → docs.
