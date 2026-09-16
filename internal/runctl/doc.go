// Package runctl is loop's shared run supervisor: it commands real
// `loop run` subprocesses on behalf of any surface — a chat room today,
// the daemon and the web UI next. One implementation, many clients.
//
// The supervisor owns the child processes and their stdins; the host
// (chat, daemon) supplies rendering through Handlers and resolves its
// own naming (a room's pipeline aliases, the daemon's run ids) into a
// fully-resolved Spec before calling Start. Deriving state from the
// child's stderr is the Translator's job — a pure state machine, so the
// wire shapes it recognizes are table-tested.
//
// runctl never imports chat or cli: rendering callbacks and durable
// transcripts belong to the host, not the core.
package runctl
