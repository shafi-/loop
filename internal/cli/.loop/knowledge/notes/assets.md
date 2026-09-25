<!-- loop area note · assets · updated 2026-09-24T16:37:19Z · coverage: assets/ assets.go · auto-maintained, verify against code -->
# Assets — Embedded assets and access layer

## What this area does
Holds template files baked into the `cli` binary at compile time via `go:embed`. Used by scaffolding commands (`loop init`, room creation) to materialize starter files into workspaces. `assets.go` is the only access point; nothing else should read the `assets/` directory directly.

## Key files
- `assets.go` — embeds two assets as strings: `assetRoom` (room.yaml template) and `assetReadme` (workspace README template). Package is `cli`; imports `embed` with blank identifier.
- `assets/room.yaml` — example/template leadership chat room: agents ceo (openai gpt-5), cfo + architect (anthropic claude-sonnet-4-5); settings `speak_threshold: 0.6`, `max_spontaneous_replies: 2`, `history_window: 50`.
- `assets/workspace-readme.md` — README written into `loop init` workspaces; documents `.loop/pipelines/` and `.loop/rooms/` and shows validate commands.

## Connections
- Consumers: workspace/project init commands and room-scaffolding code in the `cli` package — they write `string(assetRoom)` / `string(assetReadme)` to disk.
- The room.yaml shape (agents with name/role/system/model, settings speak_threshold/max_spontaneous_replies/history_window) must stay in sync with the room config parser and validator.
- README references commands (`loop validate`, `loop chat`, `loop run`, `loop init`) — update if CLI command names change.

## Gotchas / invariants
- `go:embed` paths are relative to `assets.go` and cannot start with `..` or be absolute; assets must live inside the package directory tree.
- Embedded as `string`, not `[]byte`; templates are written verbatim — no Go templating applied at embed time.
- Adding an asset requires a new `//go:embed` var; files in `assets/` are NOT auto-discovered.
- room.yaml is user-facing documentation-as-example: the settings keys shown there are part of the effective public API surface of the room config format.
- Blank `_ "embed"` import is required even when only using `//go:embed` directives.