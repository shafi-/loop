# agency-intake — the full loop story

This is loop's flagship demo: **judgment where models speak,
determinism where work runs**, in one continuous flow.

A fuzzy client request enters a chat room. An intake team interrogates
it — tag `@lead` and the account lead asks the questions; the
strategist and estimator read along and speak only when their domain is
at stake. When the room settles, the transcript becomes the input of a
delivery pipeline: brief, plan, your approval, revisions, and a shipped
plan file. The hand-off between the two halves is a file — nothing is
copy-pasted, everything is auditable.

## Prerequisites

- One provider key in your environment or `.env` (`ANTHROPIC_API_KEY`
  or `OPENAI_API_KEY` — see the [manual](../../USER%20MANUAL.md)).
- Node.js ≥ 22 for the `plan` stage (the agent runs on the cline
  executor). Everything else in the demo needs only the binary.

## Act one — the room

Open the room with a deliberately fuzzy request:

```bash
loop chat examples/agency-intake/intake-room.yaml \
  "we need a landing page for our drone photography business, something that feels premium"
```

You are the client. `@lead` will ask questions — answer like a client
would (vague is fine, that's the point). Watch two things:

- **Untagged agents decide for themselves.** The strategist and
  estimator only speak when their domain is at stake. The 👁 line shows
  who read your message and stayed silent.
- **The transcript is the artifact.** Every line lands in
  `.loop/rooms/agency-intake/transcript.jsonl`.

When the brief feels settled — goal, audience, scope, constraints on
the table — type `/quit`.

## Act two — the pipeline

```bash
loop run examples/agency-intake/delivery-pipeline.yaml
```

Stage by stage:

| Stage | What happens |
|---|---|
| `intake` | Loads the room transcript. Run this before the room exists and it fails with instructions, not mystery. |
| `brief` | Distills the raw transcript into a structured brief. Unsettled things land under *Open questions* — never invented. |
| `plan` | The planner persona (an agent with tools) drafts the delivery plan. |
| `approval` | The run **pauses** and shows you the plan. It's a gate: reply `yes`, `no`, or describe changes — free-form is understood (one small classification call reads your intent). |
| `route` | Your intent is law: `yes` ships, `no` ends the run rejected, changes revise. |
| `revise` → `back` | Your words are applied and you review again — revisions are never self-approved. |
| `ship` | Writes the approved plan to `deliveries/plan.md`. |

Interrupt it anywhere (ctrl-c) and it stays resumable:

```bash
loop run examples/agency-intake/delivery-pipeline.yaml --resume <run-id>
```

## What this demo is showing

- **Rooms and pipelines are one system.** The room's output is a file
  the pipeline reads as its first stage — judgment produces the input,
  determinism produces the deliverable.
- **The grand rule in practice.** No model blocks anywhere in these
  files: every agent and stage follows the provider configured in
  `.env`. Pin a `model:` block only where you want to override.
- **Humans are stages.** Approval is not a comment thread; it's a
  stage the run waits on, and its answer drives a router.
- **Everything is auditable.** `.loop/runs/<id>/` holds the pipeline
  snapshot and every stage result; `.loop/rooms/agency-intake/` holds
  the conversation that started it all.

## Files

- [`intake-room.yaml`](intake-room.yaml) — the room (act one)
- [`delivery-pipeline.yaml`](delivery-pipeline.yaml) — the pipeline (act two)
