# incident-room — a demo room for loop

A standing incident-response team in a chat room, owning a `respond`
pipeline with a human gate. This example is the fastest way to feel
what loop rooms do:

- **CEO-first engagement** — talk to the room *without* tagging anyone;
  the commander, the SRE and the comms lead each answer from their own
  seat (silence only for "ok" and "thanks").
- **A room that commands pipelines** — `/run respond` streams its
  status into the conversation; the human gate surfaces as a question
  you answer right there (`/approve yes`, or the buttons in the web UI).
- **Agents that know where the records are** — ask for a run's status
  and the SRE reads `state.json` / `events.jsonl` from the run dir
  instead of guessing.
- **The sidecar** — every run from this room is listed in the room
  page's side panel, expandable to its timeline.

The three personas are deliberately different: the commander drives,
the SRE answers with facts and numbers, comms translates for humans.
Tag one for a focused answer (`@sre …`), or just ask the room.

## Run it

Requires a configured provider (`.env` with `ANTHROPIC_API_KEY` or
`OPENAI_API_KEY`) and a built loop binary. From this directory:

```bash
loop validate room.yaml
loop validate respond.yaml
```

**Terminal + browser (the full experience):**

```bash
loop serve        # terminal 1 — the daemon hosting everything
loop ui           # terminal 2 — http://127.0.0.1:8787
```

In the browser: **open a room** → `room.yaml`. Then:

1. Type without tagging: *"we're seeing checkout errors spike — what
   do we watch out for?"* — the personas answer from their seats.
2. In the composer, hit `run` (the `deliver`-style alias list shows
   `respond`) — or type `/run respond`. The gate appears in the chat;
   answer with the buttons or `/approve yes`.
3. Expand the sidecar entry to see the run's timeline; ask
   `@sre, what's the status of the last run?` and watch it read the
   run records.

**Terminal only:**

```bash
loop chat --daemon room.yaml   # attach; /detach leaves the room living
# … or the classic in-process session:
loop chat room.yaml
```

## The pipeline

`respond.yaml` walks the demo flow — triage, a human gate, then two
terminal endings: `mitigate` when approved, `monitor` when not. Both
endings are `terminal: true`, so no guard router is needed. Vars pass
through: `/run respond --var incident="payments timeout"`.

## Files

| file | what it is |
|---|---|
| `room.yaml` | the team: commander, sre, comms + the owned pipeline |
| `respond.yaml` | triage → gate → mitigate/monitor |

See also `examples/agency-intake/` (a brief-to-delivery room) and the
USER MANUAL's rooms chapter for the full command surface.
