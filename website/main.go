// Command website builds loop's GitHub Pages site into -out: the landing
// page and the user manual rendered from the repo's own USER MANUAL.md —
// one source of truth for docs, no site builder, no Node toolchain.
//
// All pages use relative asset and link paths, so the output serves
// unchanged from a project base path (/loop/), a custom domain, or a
// local file server.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"html"
	"html/template"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

// tocItem is one sidebar entry of the docs table of contents.
type tocItem struct {
	Level int // heading level (2 or 3)
	Text  string
	Href  string // anchor link relative to the docs page
}

// pageData is the payload both page templates render.
type pageData struct {
	Root   string // relative prefix to the site root: "" or "../"
	Title  string
	Active string        // nav item to mark: "" (home) or "docs"
	Body   template.HTML // page content
	TOC    []tocItem     // docs sidebar; empty on the landing page
}

func main() {
	out := flag.String("out", "_site", "output directory")
	flag.Parse()
	if err := run(*out); err != nil {
		log.Fatal(err)
	}
}

func run(out string) error {
	landingT, err := template.ParseFiles(filepath.Join("website", "landing.tmpl"))
	if err != nil {
		return err
	}
	docsT, err := template.ParseFiles(filepath.Join("website", "docs.tmpl"))
	if err != nil {
		return err
	}
	css, err := os.ReadFile(filepath.Join("website", "site.css"))
	if err != nil {
		return err
	}

	manual, err := os.ReadFile("USER MANUAL.md")
	if err != nil {
		return err
	}
	body, toc, err := renderManual(manual)
	if err != nil {
		return err
	}

	write := func(rel, tmpl string, data pageData) error {
		path := filepath.Join(out, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		t := landingT
		if tmpl == "docs" {
			t = docsT
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data); err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		return os.WriteFile(path, buf.Bytes(), 0o644)
	}

	if err := write("index.html", "landing", pageData{
		Root:  "",
		Title: "loop — a deterministic agentic harness",
	}); err != nil {
		return err
	}
	if err := write(filepath.Join("docs", "index.html"), "docs", pageData{
		Root:   "../",
		Title:  "loop — user manual",
		Active: "docs",
		Body:   body,
		TOC:    toc,
	}); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "site.css"), css, 0o644); err != nil {
		return err
	}
	// Pages' Jekyll pass would ignore files it doesn't know; we have none
	// of the paths it chokes on, but the marker is free insurance.
	return os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o644)
}

// renderManual renders the manual's markdown and harvests its heading
// tree for the sidebar. Goldmark assigns auto heading IDs at render
// time (not on the AST), so the TOC is harvested from the rendered
// HTML — the slugs are exact by construction, dedupe included, and
// match the GitHub-style anchors the source's intra-document links use.
func renderManual(src []byte) (template.HTML, []tocItem, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return "", nil, err
	}
	rendered := buf.String()

	var toc []tocItem
	for _, m := range headingRe.FindAllStringSubmatch(rendered, -1) {
		text := html.UnescapeString(strings.TrimSpace(tagRe.ReplaceAllString(m[3], "")))
		if text == "" {
			continue
		}
		level := 2
		if m[1] == "h3" {
			level = 3
		}
		toc = append(toc, tocItem{Level: level, Text: text, Href: "#" + m[2]})
	}
	return template.HTML(rendered), toc, nil
}

// headingRe captures every rendered h2/h3 with its generated id.
var headingRe = regexp.MustCompile(`(?s)<(h[23]) id="([^"]+)">(.*?)</h[23]>`)

// tagRe strips inline markup (code spans, emphasis) from heading text.
var tagRe = regexp.MustCompile(`</?[a-z]+[^>]*>`)
