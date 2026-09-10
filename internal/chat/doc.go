// Package chat orchestrates multi-agent rooms: tagged agents reply first
// (forced), untagged agents observe concurrently and may speak in
// priority order up to the anti-pile-on cap. Owns transcript persistence.
package chat
