package webui

import (
	"bytes"
	"hash/fnv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// md is the chat renderer. Raw HTML in the source is escaped (goldmark's
// default) — transcript text is LLM/user output and must never inject
// markup. GFM adds the tables and strikethrough models actually emit;
// hard wraps match chat-app expectations, where a single newline breaks.
var md = goldmark.New(
	goldmark.WithExtensions(extension.Strikethrough, extension.Table),
	goldmark.WithRendererOptions(html.WithHardWraps()),
)

// renderMarkdown renders chat text as safe HTML. A conversion failure
// (theoretically impossible for any input) falls back to nothing rather
// than dropping the message.
func renderMarkdown(src string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return ""
	}
	return strings.TrimSpace(buf.String())
}

// hue maps a name to a stable hue (0-359): every persona gets its own
// avatar and accent color, consistent across refreshes and restarts.
func hue(name string) int {
	h := fnv.New32a()
	h.Write([]byte(name))
	return int(h.Sum32() % 360)
}

// initial returns the first grapheme of a name, uppercased, for the
// avatar chip.
func initial(name string) string {
	if name == "" {
		return "?"
	}
	r, _ := utf8.DecodeRuneInString(name)
	return string(unicode.ToUpper(r))
}
