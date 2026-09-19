package config

// Every artifact loop reads or writes for a workspace lives under one
// dot-directory, so a loop workspace stays a single moveable, ignorable
// unit inside someone else's repository: room and pipeline YAML, the
// project persona library, the plans the architect writes, room
// transcripts (.loop/rooms/<name>/transcript.jsonl), run records, and
// the knowledge layer. The one exception is the user-wide library,
// ~/.loop/personas — global by definition (GlobalPersonaDir).

// LoopDir is the workspace's loop folder.
const LoopDir = ".loop"

// Conventional directories inside LoopDir, relative to the workspace
// root — the directory loop commands run from.
const (
	RoomsDir     = LoopDir + "/rooms"
	PipelinesDir = LoopDir + "/pipelines"
	PersonasDir  = LoopDir + "/personas"
	PlansDir     = LoopDir + "/plans"
)
