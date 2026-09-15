# The loop pipeline contract

A pipeline is one JSON object:

```json
{
  "name": "kebab-case-name",
  "description": "one line, optional",
  "vars": { "key": "value" },
  "personas": [ { "name": "kebab-case", "role": "...", "system": "...", "model": { ... } } ],
  "stages": [ ... ],
  "runtime": { "narrator": { "model": { ... } } }
}
```

Execution is linear through `stages` in order. Every stage has:

- `id` — required, unique, lowercase letters/digits/`-`/`_`
- `type` — required, one of: `llm`, `agent`, `tool`, `human`, `router`
- `retry` — optional `{ "max_attempts": N, "backoff_ms": N }`
- `on_error` — optional, `"halt"` (default) or `"skip"`
- `output` — optional context key; defaults to the stage `id`

Stage payloads by type — no other fields are allowed:

1. `llm` — one completion. Fields: `model` (required block; the `model` id inside may be omitted when the ANTHROPIC_MODEL / OPENAI_MODEL env is set), `prompt` (required, template), `system`, `output`.
2. `agent` — a persona with tools working until done, run by a pluggable executor. Fields: `persona` (required, must appear in `personas`), `input` (template), `executor` (optional, default `cline`), `approval` (`"auto"` default or `"ask"`), `model` (optional override), `tools` (only: `read_file`, `write_file`, `run_command`), `max_iterations`, `output`.
3. `tool` — deterministic local command. Fields: `run` (required), `input` (piped to stdin), `env` (object), `output`. Captured stdout becomes the stage output.
4. `human` — pauses and asks the user. Fields: `prompt` (required, template), `output` (defaults to `"<id>.answer"`).
5. `router` — branches; overrides linear flow. Field: `when` (required): ordered array of `{ "if": "<expr>", "next": "<stage id>" }`. The final rule MUST omit `if` — it is the default arm. `next` may jump forward (skip) or backward (rework loop).

`model` objects (only these two providers exist; the `model` id is optional — env overrides then built-in defaults apply):

```json
{ "provider": "anthropic", "model": "claude-sonnet-4-5", "base_url": "optional gateway speaking Anthropic's API" }
{ "provider": "openai", "model": "gpt-5", "base_url": "optional, for compatible services" }
```

Optional: `api_key_env` (defaults to `ANTHROPIC_API_KEY`/`OPENAI_API_KEY`), `temperature` (0–2), `max_tokens`.

Templates reference the run context: `{{ vars.idea }}` for pipeline vars,
`${stages.<id>.output}` for a prior stage's output, `${stages.<id>.answer}`
for a human stage's answer.
