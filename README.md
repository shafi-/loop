# loop

**loop** is a deterministic agentic harness in a single Go binary.

- **Pipelines** — describe work as YAML: ordered stages (`llm`, `agent`,
  `tool`, `human`, `router`), deterministic flow, a full audit trail, and
  resume-after-failure.
- **Rooms** — talk to teams of persona agents: tag `@name` and it must
  reply; untagged agents read along and decide for themselves whether to
  speak.

Two halves, one idea: **determinism where work runs, judgment where
models speak.**

## Install

One line (macOS / Linux, amd64 or arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/shafi-/loop/main/install.sh | sh
```

Or build from source:

```bash
go build -o loop ./cmd/loop
```

## Quickstart

Run the onboarding command — it checks your provider config, installs
the agent executor (fetching a self-contained toolchain if needed, so
**no Node.js required afterward**), and health-checks the result:

```bash
loop setup
```

Set one key if you haven't — everything else (provider, model,
endpoint) resolves from it, per the [grand rule](USER%20MANUAL.md#the-grand-rule):

```bash
export ANTHROPIC_API_KEY=sk-...   # or OPENAI_API_KEY; gateways: see the manual
```

Scaffold a workspace and run your first pipeline:

```bash
loop init
loop run pipelines/feature-delivery.yaml
```

The example includes an `agent` stage; `loop setup` made it runnable.
Then open a room:

```bash
loop chat rooms/leadership.yaml "We need to have a new drone shoot out"
```

## The full story

[`examples/agency-intake/`](examples/agency-intake/) is the flagship
demo: a client request enters a chat room, an intake team interrogates
it, and the room's settled transcript becomes the input of a delivery
pipeline — judgment first, determinism after.

## Workspace layout

A loop workspace is just a directory:

```
pipelines/   deterministic stage workflows   (loop run <file>)
rooms/       multi-agent chat rooms          (loop chat <file>)
.loop/       run logs, transcripts, resume state (created on demand)
.env         keys and provider config; never committed
```

## Documentation

The full reference — every command, the YAML schemas for pipelines and
rooms, model resolution, executors, resume, troubleshooting — lives in
[USER MANUAL.md](USER%20MANUAL.md).

## License

[MIT](LICENSE)
