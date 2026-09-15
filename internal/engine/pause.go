package engine

import "errors"

// ErrPaused is returned by a HumanIO when the user asked to stop the
// run at a prompt (/pause, /quit, /exit). The Runner treats it as a
// clean, resumable stop — not a failure: state records the paused
// stage so resume re-runs it, and the exit is quiet.
var ErrPaused = errors.New("run paused at your request")
