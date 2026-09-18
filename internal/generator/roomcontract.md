# The loop room contract

A chat room is one JSON object:

```json
{
  "name": "kebab-case-name",
  "pipelines": [ { "name": "alias", "file": "./relative-or-workspace-path.yaml" } ],
  "agents": [ { "name": "kebab-case", "role": "...", "system": "...", "tools": ["read_file"] } ],
  "settings": { "speak_threshold": 0.6, "max_spontaneous_replies": 2, "history_window": 50 }
}
```

A room is a standing team the user talks to in chat. The user is the
boss: messages addressed with `@name` force that agent to reply; every
other agent reads along and independently decides whether to speak,
based on its role. Design the team like real senior people:

- `name` — required, unique, lowercase letters/digits/`-`/`_`.
- `agents` — required, 2–5 members. An agent is either concrete or a
  library reference:
  - **concrete** — `name`, `system` (required, written in second person:
    "You are ..."). It must state the agent's seat (what it cares
    about), how it answers (facts, numbers, directives, plain
    language...), and above all **when to speak and when to stay
    silent**. Silence is each agent's default; give every agent a
    concrete trigger for contributing. `role` is a short title.
  - **reference** — `{ "persona": "<name>" }` pulls a persona from the
    workspace's library. Use a reference ONLY for names listed under
    AVAILABLE PERSONAS, and never set `name`, `role`, or `system` next
    to it — the library provides them. `tools` may specialize it.
  - `tools` — optional array; only `read_file`, `write_file`,
    `run_command` exist as grants (project_notes is auto-granted to
    every tool-using agent). Grant tools only when the agent genuinely
    needs to read or persist files (e.g. an analyst that saves its
    findings).
- `pipelines` — omit unless the description explicitly names a pipeline
  to command. `name` is the in-room alias users type after `/run`;
  `file` points at the pipeline YAML.
- `settings` — optional; omit for defaults. `speak_threshold` 0–1 (how
  convinced an untagged agent must be to speak),
  `max_spontaneous_replies` (anti-pile-on cap per message),
  `history_window` (transcript lines each observer sees).

NEVER emit `model` objects — agents always follow the provider family
configured in the environment. Seats should be genuinely different
people: distinct mandates, distinct answers, minimal overlap.
