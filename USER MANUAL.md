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

### Install

One line (macOS / Linux, amd64 or arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/shafi-/loop/main/install.sh | sh
```

The script picks the right binary from the latest GitHub release,
verifies its SHA256 checksum, and installs to `/usr/local/bin`
(fallback: `~/.local/bin`; override with `INSTALL_DIR=/some/path`).

Or build from source:

```bash
go build -o loop ./cmd/loop      # from a checkout of this repository
```

You only need **Node.js ≥ 22** if you use `agent` stages (they run on the
cline executor, a Node sidecar). Pipelines made only of `llm`, `tool`,
`human`, and `router` stages need nothing but the binary. Check what your
installation can do with `./loop doctor`.

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
| `LOOP_COUNTERS` | Opt in to anonymous, local-only usage counters (`1` to enable). See [Counters](#counters-opt-in-anonymous-local). |
| `LOOP_COUNTERS_FILE` | Override the counters file location (default `~/.loop/counters.json`). |

Endpoint conventions differ by family — this is deliberate:

- **anthropic**: the base carries **no** `/v1`. Requests go to `<base>/v1/messages`. (e.g. Z.ai: `https://api.z.ai/api/anthropic`)
- **openai**: the base is used **as-is**, version prefix included. Requests go to `<base>/chat/completions`. (e.g. Ollama: `http://localhost:11434/v1`)

### Check your setup

```bash
./loop doctor
```

Reports `.env` presence, node version, cline executor state, and
whether counters are on.

---

## 2. Quickstart

```bash
./loop init                      # scaffold pipelines/ + rooms/ + README
./loop validate pipelines/feature-delivery.yaml
./loop run pipelines/feature-delivery.yaml
```

The example pipeline turns an idea into a reviewed implementation plan:
an `llm` stage writes requirements, an `agent` stage (the `architect`
persona, with tools) designs the system, and a **gated** `human` stage
pauses for your approval — reply `yes`, `no`, or describe what should
change; free-form is understood (one classification call reads your
intent). A `router` branches on that intent: `yes` proceeds to build,
`no` ends the run rejected, and change requests feed a `revise` stage
that asks you again. Rejections never redo the upstream stages; they
feed your words forward.

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
(participants with their resolved provider/model and tools), `/help`,
`/quit` — and, when the room owns pipelines (§5), `/pipelines`,
`/run`, `/approve`, `/status`, `/halt`.
Transcripts persist under `.loop/rooms/<room>/transcript.jsonl`.
`--rooms-dir` moves that location.

### `loop executor install <name>` / `loop executor doctor` / `loop doctor`

`install cline` writes the Node host to `~/.loop/executors/cline/` and
runs `npm install` there. Re-run it after upgrading loop. `doctor`
reports node version, executor health, and counters state.

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

Every stage also takes `id`, `retry`, `on_error` — and `terminal: true`:
completing a terminal stage **ends the run** (marked done, not
resumable). Terminals let a pipeline have several ending branches — a
ship stage and a reject stage — without guard routers after each one,
because a jumped-to stage otherwise flows linearly onward. Not valid on
routers.

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
verbatim for routing. At any human prompt: **`/pause`, `/quit`, or
`/exit`** stops the run cleanly (state saved, exit 0, resumable —
resume re-runs the prompt); an **empty line re-asks** instead of
counting as an answer. Both the question and your answer are recorded
in the run's `events.jsonl`.

**`gate: true`** on a human stage turns the answer into an understood
intent. The gate offers three kinds of answer:

- **yes / no** (or `y` / `n`) — recognized directly and deterministically.
  No model is involved, so even a keyless environment gates crisp answers.
- **words** — anything else gets **one** classification call that sees
  the question you were asked (artifact included) and your words, and
  labels the intent. If the classifier misbehaves, the gate falls back
  to `changes` — the safe loop, never a misread approval.

The label lands in the context as `stages.<id>.intent`
(`yes` | `no` | `changes`); your raw words stay in
`stages.<id>.answer` (that's what a revise stage consumes). Routers
branch on the intent, exactly matched — determinism where work runs,
judgment where models speak. An optional `model:` on the stage
configures the classification call (otherwise env-driven; the field is
only legal with `gate: true`).

```yaml
- id: approval
  type: human
  gate: true
  prompt: |
    Review the plan below. Reply yes to approve, no to reject, or
    describe what should change — free-form is understood.

    ${outputs.plan_md}
- id: route
  type: router
  when:
    - if: "${stages.approval.intent} == 'yes'"
      next: ship
    - if: "${stages.approval.intent} == 'no'"
      next: rejected
    - next: revise
```

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
- `stages.<id>.output` — any stage's result (`.output` may be omitted);
  frozen at that stage's own run
- `stages.<id>.answer` — a human stage's raw reply
- `stages.<id>.intent` — a gate's classified intent (`yes` | `no` |
  `changes`), when the human stage sets `gate: true`
- `outputs.<name>` — a stage's alias when it sets an explicit `output`;
  shared and **mutable** — a revise stage writing the same alias
  overwrites it, so references always see the latest (the revision-loop
  idiom)

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

pipelines:                      # pipelines this room can command
  - name: deliver               # in-room alias
    file: ./delivery.yaml       # resolved relative to the room file

agents:
  - name: ceo                   # address with @ceo
    role: CEO
    system: |
      You are the CEO. ...
    # no model block → follows the configured env family
  - name: architect
    role: Architect
    system: |
      You design systems. ...
    tools: [read_file, write_file]   # optional — see "agents with tools"

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

### Agents with tools

`tools:` on a room agent enables a bounded native tool loop inside its
replies: `read_file`, `write_file`, `run_command` (the fixed set).
File paths are **confined to the workspace** — absolute paths and `..`
escapes are refused; tool output is capped; commands run via `sh -c`
with a 60s timeout; the loop is bounded (8 rounds). Every executed tool
is noticed in the room (`🔧 @architect wrote plans/x.md`) and recorded
in the transcript, so the other agents learn the file exists. Tool
errors go back to the model as results — a bad path is a correction,
not a dead turn. Caveat worth knowing: `run_command` executes what the
model asks; grant it only to agents you trust with a shell.

### Rooms command pipelines

A room with a `pipelines:` section is a cockpit. In the session:

| Command | What it does |
|---|---|
| `/pipelines` | list owned pipelines |
| `/run <name> [--var k=v]…` | run one as a **real `loop run` subprocess**, in the background |
| `/approve <yes\|no\|words>` | answer a pipeline asking for approval (free words understood — the gate contract) |
| `/status` | active runs: alias, run id, state, last event |
| `/halt [name]` | stop a run cleanly — resumable with `/run <name> --resume <id>` |

The run pushes **status one-liners** into the room as they happen
(`▸ deliver: → brief (llm)`); its **output stays in files**
(`.loop/runs/<id>/` and stage-written artifacts) — the room never gets
a wall of stage output. Gates ask **in the room**; your `/approve`
answer is fed to the run, where the same terminal semantics apply
(vocabulary yes/no, one classification call for words, `/pause`
equivalents via `/halt`). Only **you** start runs; agents can propose,
nothing spends without your command.

Rules of the road: one active run per pipeline alias, several aliases
concurrently; when several gates wait at once, name the target
(`/approve deliver yes`, `/halt deliver`); quitting the room halts all
runs resumably. Concurrent pipelines writing the **same workspace
paths** can collide — that's a property of the pipelines, same as two
terminals.

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
- `events.jsonl` — every stage event: executor activity **with its text
  content**, human questions and answers, router decisions, narrations
- `context.json` — the accumulated run context (last snapshot)
- `state.json` — executed path, stop point (failed or paused stage),
  completion state

`--resume <id>` restores the context, **replays the recorded path in
order without re-executing it**, re-runs only the stop point (the
failed stage, or the prompt where you paused), then continues with
fresh stages. Deterministic: earlier stages produce identical inputs,
the re-run stage gets a clean retry.

A run announces its id at start; keep it, or pass your own with
`--run-id`. Stopping mid-run is always safe: `/pause` (or `/quit`,
`/exit`) at a prompt, or ctrl-c anywhere — both record where to resume.

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
| `~/.loop/counters.json` | opt-in usage counters (see below) |
| `.env` | your credentials (gitignored — keep real keys here, never in YAML) |

### Counters (opt-in, anonymous, local)

loop can keep simple usage counts — workspaces initialized, pipelines
generated, pipeline runs and reruns (per pipeline name), room sessions.
This is **off by default**; set `LOOP_COUNTERS=1` to opt in.

What you get is plain numbers in `~/.loop/counters.json` (override with
`LOOP_COUNTERS_FILE`): no content, no identifiers, no machine info —
and nothing ever leaves your machine; there is no network call. A
counters problem (unreadable file, unwritable directory) is silently
ignored: counting can never break the command it counts for.
`loop doctor` shows whether counters are on and, if so, the current
tallies.
