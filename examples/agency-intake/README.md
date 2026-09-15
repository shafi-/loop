# agency-intake — the full loop story

This is loop's flagship demo: **judgment where models speak,
determinism where work runs**, in one continuous flow — and the room
is the cockpit.

A fuzzy client request enters the chat room. An intake team interrogates
it — tag `@lead` and the account lead asks the questions; the strategist
and estimator read along and speak only when their domain is at stake.
When the room settles, **you command the delivery pipeline from inside
the conversation** (`/run deliver`): it executes as a real background
`loop run`, reports status back to the room, and asks for your approval
in the room when it matters. Output lands in files; the conversation
stays clean.

## Prerequisites

- One provider key in your environment or `.env` (`ANTHROPIC_API_KEY`
  or `OPENAI_API_KEY` — see the [manual](../../USER%20MANUAL.md)).
- Node.js ≥ 22 for the `plan` stage (the agent runs on the cline
  executor). Everything else needs only the binary.

## Act one — the room settles the brief

Open the room with a deliberately fuzzy request:

```bash
cd examples/agency-intake
loop chat intake-room.yaml \
  "we need a landing page for our drone photography business, something that feels premium"
```

You are the client. `@lead` will ask questions — answer like a client
would (vague is fine, that's the point). Watch three things:

- **Untagged agents decide for themselves.** The strategist and
  estimator only speak when their domain is at stake. The 👁 line shows
  who read your message and stayed silent.
- **The strategist can persist files.** Ask it to write the settled
  brief (e.g. `@strategist persist the brief to briefs/drone.md`) —
  its `read_file`/`write_file` tools are confined to the workspace, and
  every write is noticed in the room and recorded in the transcript.
- **The transcript is the artifact.** Every line lands in
  `.loop/rooms/agency-intake/transcript.jsonl`.

## Act two — command the delivery from the room

The room owns the delivery pipeline. When the brief feels settled —
goal, audience, scope, constraints on the table:

```
/run deliver
```

The run executes as a **real `loop run` subprocess** in the background;
the conversation keeps working while it runs. What you see:

| What | Where |
|---|---|
| Status one-liners (`▸ deliver: → brief (llm)` …) | pushed into the room as they happen |
| Stage output (the plan, the brief) | files: `.loop/runs/<id>/`, `deliveries/plan.md` |
| Approval gates | asked **in the room** — answer with `/approve yes` · `/approve no` · `/approve <words>` (free-form is understood) |
| On demand | `/status` shows active runs and their run ids |

Free words at `/approve` get one classification call — the same gate
contract as at the terminal. `/halt` stops a run cleanly;
`/run deliver --resume <id>` picks it back up. Several owned pipelines
can run at once (one run per pipeline); when several gates wait at the
same time, name the pipeline: `/approve deliver yes`.

## What this demo is showing

- **Rooms and pipelines are one system, commanded from one place.** The
  room's conversation produces the input; `/run` turns judgment into
  deterministic execution; status flows back; approval flows forward.
- **The noise contract.** The room carries conversation and status —
  never a wall of stage output. Artifacts live in files and run logs.
- **Agents with tools, bounded.** The strategist writes files inside
  the workspace only; every call is audited in the transcript.
- **The grand rule in practice.** No model blocks anywhere in these
  files: every agent, stage, and gate follows the provider configured
  in `.env`. Pin a `model:` block only where you want to override.
- **Humans are stages.** Approval is a gate the run waits on — whether
  you answer at the terminal or with `/approve` in the room.
- **Everything is auditable.** `.loop/runs/<id>/` holds the pipeline
  snapshot and every stage result; `.loop/rooms/agency-intake/` holds
  the conversation that started it all.

## Files

- [`intake-room.yaml`](intake-room.yaml) — the room: the team, its tools, and the `deliver` pipeline it owns
- [`delivery-pipeline.yaml`](delivery-pipeline.yaml) — the pipeline: intake → brief → plan → gate → ship
