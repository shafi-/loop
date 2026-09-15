# loop — User Manual

**loop** is a deterministic agentic harness: you describe work as YAML
pipelines (ordered stages, deterministic flow, full audit trail), and you
talk to teams of persona agents in chat rooms (tag an agent and it must
reply; untagged agents read along and decide for themselves whether to
speak). A single Go binary, CLI-first.

Two halves, one idea: **determinism where work runs, judgment where
models speak.**

---

## 1. Setup

### Build

```bash
go build -o loop ./cmd/loop      # from a checkout of this repository
```

You only need **Node.js ≥ 22** if you use `agent` stages (they run on the
cline executor, a Node sidecar). Pipelines made only of `llm`, `tool`,
`human`, and `router` stages need nothing but the binary.

### Configure credentials

```bash
cp .env.sample .env      # then fill in the keys you use
```

`.env` is loaded on every command, from your working directory. Your real
shell environment always wins over the file (the file only fills gaps).
Set just **one** family and everything follows it.

### The grand rule

Everything loop resolves — provider, model, endpoint, key — follows one
precedence, per field:

```
explicit YAML / flag  >  environment (shell > .env)  >  built-in default
```

Overrides are **partial**: name only a `model:` in YAML and the family,
endpoint, and key still come from the environment; name only a
`provider:` and the model id still comes from the environment. A config
may also be fully self-contained with no environment at all.

### Environment variables

| Variable | Purpose |
|---|---|
| `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` | API keys (defaults per family) |
| `PROVIDER` | Your stated family (`anthropic` or `openai`). Only needed when **both** families are configured; otherwise the single configured family is picked automatically. Explicit YAML still wins over it. |
| `ANTHROPIC_MODEL` / `OPENAI_MODEL` | Model override. Defaults: `claude-sonnet-4-5` / `gpt-5`. |
| `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL` | Endpoint override (gateway, proxy, local server). Unset = official endpoint. Keyless local endpoints need no key. |
| `LOOP_ENV_FILE` | Path to an additional env file, loaded after `./.env`. |
| `LOOP_NODE` | Node binary for the cline executor (default `node` on PATH; needs ≥ 22). |
| `LOOP_CLINE_HOST` | Override the host script location (default `~/.loop/executors/cline/index.mjs`). |

Endpoint conventions differ by family — this is deliberate:

- **anthropic**: the base carries **no** `/v1`. Requests go to `<base>/v1/messages`. (e.g. Z.ai: `https://api.z.ai/api/anthropic`)
- **openai**: the base is used **as-is**, version prefix included. Requests go to `<base>/chat/completions`. (e.g. Ollama: `http://localhost:11434/v1`)

### Check your setup

```bash
./loop doctor
```

Reports `.env` presence, node version, and cline executor state.

---

## 2. Quickstart

```bash
./loop init                      # scaffold pipelines/ + rooms/ + README
./loop validate pipelines/feature-delivery.yaml
./loop run pipelines/feature-delivery.yaml
```

The example pipeline turns an idea into a reviewed implementation plan:
an `llm` stage writes requirements, an `agent` stage (the `architect`
persona, with tools) designs the system, a `human` stage pauses for your
approval, a `router` branches on your answer.

Chat with the leadership room:

```bash
./loop chat rooms/leadership.yaml "@architect evaluate: is this feasible?"
```

Tag an agent → it must reply. Untagged agents judge whether they should
speak; when they decline you'll see `👁 @ceo @cfo saw the message`.

Smoke-test your provider configuration:

```bash
./loop ask "In one sentence, introduce yourself."
```

---

## 3. Command reference

### `loop validate <file>...`

Validate pipeline or room YAML. Unknown fields are errors; semantics
(duplicate ids, bad routing targets, invalid providers) are checked
collectively — you see every problem at once. `--type` forces
`pipeline` or `room` instead of auto-detection.

### `loop init [dir] [--force]`

Scaffold a workspace: `pipelines/feature-delivery.yaml`,
`rooms/leadership.yaml`, and a `README.md`. Existing files are skipped
unless `--force`.

### `loop ask <prompt>`

One prompt → one streamed answer. The provider smoke test.

Flags: `--provider`, `--model`, `--base-url`, `--api-key-env`,
`--system`, `--no-stream`, `--retries N` (default 3).

### `loop new "<description>"`

Generate a pipeline from a plain-language description. An LLM drafts the
pipeline JSON, loop's own validator checks it, and validation errors are
fed back to the model for repair (default 2 repair passes). You get
validated YAML — or an honest failure. Hand-written pipelines are
equally first-class.

Flags: `-o/--out` (file or directory; default stdout), `--provider`,
`--model`, `--base-url`, `--api-key-env`, `--max-repairs`, `-q/--quiet`.

The generated pipeline targets the same provider family you're drafting
with (resolved from flags/env).

### `loop run <pipeline.yaml>`

Execute a pipeline. Progress goes to stderr (stage arrows, narrator
lines, failure summaries); stage text streams to stdout. On failure you
get the stage, the parsed provider error with a hint, and a resume id.

Flags: `--run-id` (choose your own id), `--resume <id>`, `--runs-dir`
(default `.loop/runs`), `-q/--quiet` (failures still print).

### `loop chat <room.yaml> [opening message]`

Open a multi-agent room. The optional opening message is delivered as if
you typed it, then the session stays interactive. Commands: `/agents`
(participants with their resolved provider/model), `/help`, `/quit`.
Transcripts persist under `.loop/rooms/<room>/transcript.jsonl`.
`--rooms-dir` moves that location.

### `loop executor install <name>` / `loop executor doctor` / `loop doctor`

`install cline` writes the Node host to `~/.loop/executors/cline/` and
runs `npm install` there. Re-run it after upgrading loop. `doctor`
reports node version and executor health.

---

## 4. Pipeline YAML reference

```yaml
name: feature-delivery          # required, kebab-case
description: one-line purpose

vars:                           # pipeline inputs; reference as {{ vars.idea }}
  idea: add OAuth login

personas:                       # reusable agent identities
  - name: architect             # kebab-case, unique
    role: Software architect
    system: |
      You are a pragmatic software architect...
    model:                      # optional — see "model blocks" below
      provider: anthropic

runtime:
  narrator: {}                  # enable the narrator (see §6); {} = env-driven

stages:
  - id: requirements            # required, kebab-case, unique
    type: llm
    model: {provider: openai}   # optional
    prompt: |
      Turn this idea into requirements.
      Idea: {{ vars.idea }}
    output: requirements_md     # optional; default = stage id
```

### Stage types

**`llm`** — one templated completion. Fields: `model` (optional),
`prompt` (required), `system`, `output`. The response text becomes the
stage output.

**`agent`** — a persona with tools working until done, run by a pluggable
executor (default `cline`). Fields: `persona` (required, must exist in
`personas`), `input` (required — the interpolated instruction; executors
receive concrete tasks, never templates), `tools`
(`read_file`, `write_file`, `run_command`), `max_iterations`,
`approval` (`auto` default or `ask`), `executor`, `model` (optional
override of the persona's), `output`.

**`tool`** — deterministic local command. Fields: `run` (required,
executed via `sh -c`), `input` (piped to stdin), `env` (object,
interpolated), `output`. Captured stdout (trailing newline trimmed)
becomes the stage output; stdout is capped at 1 MiB.

**`human`** — pauses and asks you in the terminal. Fields: `prompt`
(required), `output` (defaults to `"<id>.answer"`; the raw answer is
also kept as `stages.<id>.answer`). With a narrator enabled, the
narrator mediates the question; your raw answer is always preserved
verbatim for routing.

**`router`** — deterministic branching; overrides linear flow. Field:
`when` (required): ordered rules `{ "if": "<expr>", "next": "<stage id>" }`;
the final rule must omit `if` (the default arm). Expressions compare an
interpolated value with `==` or `!=` against a quoted or bare literal.
Targets may jump forward (skip) or backward (rework loops).

### Model blocks (optional, partially overridable)

```yaml
model:
  provider: anthropic           # optional — env PROVIDER / inference applies
  model: claude-sonnet-4-5      # optional — ANTHROPIC_MODEL / OPENAI_MODEL, then default
  base_url: https://gw.example  # optional — per family; see endpoint conventions
  api_key_env: MY_KEY           # optional — non-default key variable
  temperature: 0.7              # optional, 0–2
  max_tokens: 4096              # optional
```

Omit the whole block, or any field in it — the environment fills the
gaps. Validation only rejects contradictions (a misspelled provider).

### Templates and context

Reference earlier results with `{{ ... }}` or `${ ... }`:

- `vars.<name>` — pipeline inputs
- `stages.<id>.output` — any stage's result (`.output` may be omitted)
- `stages.<id>.answer` — a human stage's raw reply
- `outputs.<name>` — a stage's alias when it sets an explicit `output`

Unknown references are **hard errors** — a deterministic run never
silently substitutes empty strings.

### Failure policy

Per stage:

```yaml
retry:
  max_attempts: 3               # total attempts (default: no retry)
  backoff_ms: 500               # base backoff, doubles per attempt
on_error: halt                  # "halt" (default) or "skip" (record + continue)
```

Provider errors are classified (rate_limited, auth, network,
model_not_found, context_too_long, overloaded, …) with actionable hints;
`Retry-After` headers are honored; mid-stream failures are never
retried (you'd see duplicated output).

---

## 5. Chat rooms YAML reference

```yaml
name: leadership

agents:
  - name: ceo                   # address with @ceo
    role: CEO
    system: |
      You are the CEO. ...
    # no model block → follows the configured env family

settings:
  speak_threshold: 0.6          # 0–1; observers above this speak
  max_spontaneous_replies: 2    # anti-pile-on cap per message
  history_window: 50            # transcript lines each observer sees
```

**How a message flows:** tagged agents reply first (mandatory, streamed).
Then every untagged agent independently runs a cheap structured
self-check — *should I speak?* — returning `{speak, priority 1–5,
reason}`. Priority maps to confidence; only decisions above
`speak_threshold` qualify, sorted by priority, capped by
`max_spontaneous_replies`. Agents that read but decline show as
`👁 @name saw the message` — silence with a receipt, never a verbose
excuse. Failed self-checks are silent (an observer that errs stays
quiet).

---

## 6. The narrator

Enable with `runtime.narrator: {}` (a full model block also works).
The narrator is loop's core-loop LLM: it emits one-line progress
comments after stages, mediates `human` prompts, and summarizes
failures. It is **best-effort**: if its own calls fail, the run
continues and you simply see fewer `·`/`ℹ` lines.

---

## 7. Run artifacts and resume

Every run writes `.loop/runs/<id>/`:

- `pipeline.yaml` — the exact snapshot that ran
- `events.jsonl` — every stage event, including executor tool activity
- `context.json` — the accumulated run context (last snapshot)
- `state.json` — executed path, failed stage, completion state

`--resume <id>` restores the context, **replays the recorded path in
order without re-executing it**, re-runs only the failed stage, then
continues with fresh stages. Deterministic: earlier stages produce
identical inputs, the failed stage gets a clean retry.

A run announces its id at start; keep it, or pass your own with
`--run-id`.

---

## 8. Executors (agent stages)

Agent stages delegate to an executor. v1 ships **cline**: a Node
sidecar (JSONL over stdio) hosting the cline agent SDK. The Go engine
owns determinism and the audit trail; the host owns the agentic loop.

```bash
./loop executor install cline   # writes ~/.loop/executors/cline + npm install
```

Requirements: Node ≥ 22. Old system node? Point at a newer one:

```bash
LOOP_NODE=/opt/homebrew/opt/node@25/bin/node ./loop run ...
```

The executor receives the fully resolved model spec (family, model,
endpoint, key — env rules apply, including per-family endpoint
conventions). Executor events stream into the run log, so agent tool
activity is auditable like everything else.

---

## 9. Troubleshooting

| Symptom | Meaning / fix |
|---|---|
| `env var X is not set (provider "..." requires it — set the key, or switch this model block...)` | A stage named a family you have no credentials for. Set the named key, add `PROVIDER`, or fix the stage's `provider:`. |
| `HTTP 503 (model_not_found): No available channel for model ...` | Your endpoint serves a different model catalog. Point `*_MODEL` at a model the endpoint actually offers. |
| `HTTP 403 (auth): credit limit insufficient` | The endpoint account is out of credit. Top up or switch endpoints. |
| `context_length_exceeded` / `prompt is too long` | The input outgrew the model's window. Shorten upstream stages or raise the model tier. |
| `node 16 is too old: the cline executor needs Node 22+` | Set `LOOP_NODE` to a modern node binary. |
| `No output generated. The model stream ended without a finish chunk` | The endpoint answered the SDK's request with a non-stream response (often a 404 behind a 200 from a gateway). Check the base URL convention: anthropic bases carry **no** `/v1`. |
| `unknown reference "stages.x.y"` | A template referenced a stage that hasn't run (or a typo). Router "when" values are interpolated before comparison. |
| Output truncated mid-text | The model hit `max_tokens`; loop logs `output_truncated` in the run log. Raise `max_tokens` on that stage. |
| A run "announces" one id but writes elsewhere | You're resuming across different `--runs-dir`s; run from the same workspace. |

Where to look next: the failing run's `events.jsonl` records every
executor event and provider detail; `.env` problems show up at startup
with file:line warnings.

---

## 10. Where things live

| Path | Contents |
|---|---|
| `.loop/runs/<id>/` | run logs, context snapshots, state |
| `.loop/rooms/<room>/` | chat transcripts |
| `~/.loop/executors/cline/` | installed cline host |
| `.env` | your credentials (gitignored — keep real keys here, never in YAML) |
