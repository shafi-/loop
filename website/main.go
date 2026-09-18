// Command website builds loop's GitHub Pages site into -out: the landing
// page and the docs rendered from the repo's own USER MANUAL.md — one
// source of truth for docs, no site builder, no Node toolchain.
//
// The docs are split into one page per manual section (Laravel-style:
// short focused pages, a sidebar, prev/next), with cross-page anchor
// links rewritten to the right page. All paths are relative, so the
// output serves unchanged from a project base path (/loop/), a custom
// domain, or a local file server.
package main

import (
	"bytes"
	"flag"
	"html"
	"html/template"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
)

// docsLink is one sidebar entry (a section page).
type docsLink struct {
	Slug  string
	Href  string // relative to the current page
	Title string
}

// docsCard is one card on the docs overview page.
type docsCard struct {
	Href, Title, Blurb string
}

// tocItem is one on-this-page subsection entry.
type tocItem struct {
	Text, Href string
}

// pageData is the payload every page template renders.
type pageData struct {
	Root     string // relative prefix to the site root
	Title    string
	Active   string // current section slug ("" on the docs overview)
	Body     template.HTML
	Pages    []docsLink // sidebar: all sections
	Subs     []tocItem  // sidebar: active page's subsections
	Sections []docsCard // overview page: section cards
	Prev     *docsLink
	Next     *docsLink
}

// blurbs are the docs overview's one-line descriptions, keyed by slug.
var blurbs = map[string]string{
	"setup":                     "Install loop, set one key, check your installation.",
	"quickstart":                "First pipeline and first room, in two minutes.",
	"loop-in-your-project":      "Adopt loop inside an existing repo — and one dashboard per project.",
	"command-reference":         "Every command and its flags.",
	"pipeline-yaml-reference":   "Stages, gates, routers, model blocks, failure policy.",
	"chat-rooms-yaml-reference": "Personas, tools, room-owned pipelines, the speak policy.",
	"the-narrator":              "Optional one-line progress commentary.",
	"run-artifacts-and-resume":  "What every run records; how resume works.",
	"executors-agent-stages":    "The cline executor and the loop setup command.",
	"troubleshooting":           "Common failures and what they mean.",
	"where-things-live":         "Workspaces, sockets, counters.",
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
	if err := buildDocs(out, docsT, manual); err != nil {
		return err
	}
	landing := pageData{Root: "", Title: "loop — plan in a room, run in a pipeline"}
	var buf bytes.Buffer
	if err := landingT.Execute(&buf, landing); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "index.html"), buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "site.css"), css, 0o644); err != nil {
		return err
	}
	// Pages' Jekyll pass would ignore files it doesn't know; we have none
	// of the paths it chokes on, but the marker is free insurance.
	if err := os.WriteFile(filepath.Join(out, ".nojekyll"), nil, 0o644); err != nil {
		return err
	}
	// Landing assets (screenshots), mirrored 1:1.
	return filepath.WalkDir(filepath.Join("website", "assets"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel("website", path)
		if err != nil {
			return err
		}
		dst := filepath.Join(out, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
}

// head is one split point: a manual section.
type head struct {
	id, title string
}

func buildDocs(out string, docsT *template.Template, manual []byte) error {
	rendered, err := renderManual(manual)
	if err != nil {
		return err
	}
	intro, heads, bodies := splitSections(rendered)

	// Page identities: "10. Where things live" → where-things-live.
	sections := make([]docsLink, len(heads))
	for i, h := range heads {
		sections[i] = docsLink{Slug: slugify(h.title), Title: h.title}
	}

	// Where every heading id lives, for cross-page anchor rewriting.
	anchorSection := map[string]string{}
	for i, b := range bodies {
		if m := sectionIDRe.FindStringSubmatch(b); m != nil {
			anchorSection[m[1]] = sections[i].Slug
		}
		for _, m := range h3TextRe.FindAllStringSubmatch(b, -1) {
			anchorSection[m[1]] = sections[i].Slug
		}
	}

	// write renders one docs page; hrefs between pages are relative to
	// the page's own directory (root is the prefix back to site root).
	write := func(dir, root, active string, body template.HTML, subs []tocItem, cards []docsCard, prev, next *docsLink) error {
		pages := make([]docsLink, len(sections))
		for i, s := range sections {
			pages[i] = docsLink{Slug: s.Slug, Title: s.Title, Href: root + "docs/" + s.Slug + "/"}
		}
		data := pageData{
			Root: root, Active: active, Body: body, Pages: pages, Subs: subs,
			Sections: cards, Prev: prev, Next: next,
		}
		var buf bytes.Buffer
		if err := docsT.Execute(&buf, data); err != nil {
			return err
		}
		path := filepath.Join(out, dir, "index.html")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, buf.Bytes(), 0o644)
	}

	// Overview: the manual's intro, then one card per section.
	cards := make([]docsCard, len(sections))
	for i, s := range sections {
		cards[i] = docsCard{Href: s.Slug + "/", Title: s.Title, Blurb: blurbs[s.Slug]}
	}
	var next *docsLink
	if len(sections) > 0 {
		next = &docsLink{Title: sections[0].Title, Href: sections[0].Slug + "/"}
	}
	if err := write("docs", "../", "", template.HTML(rewriteAnchors(intro, "", "", anchorSection)), nil, cards, nil, next); err != nil {
		return err
	}

	// One focused page per section.
	for i, s := range sections {
		var prev, next *docsLink
		if i > 0 {
			prev = &docsLink{Title: sections[i-1].Title, Href: "../" + sections[i-1].Slug + "/"}
		}
		if i+1 < len(sections) {
			next = &docsLink{Title: sections[i+1].Title, Href: "../" + sections[i+1].Slug + "/"}
		}
		dir := filepath.Join("docs", s.Slug)
		if err := write(dir, "../../", s.Slug,
			template.HTML(rewriteAnchors(bodies[i], s.Slug, "../", anchorSection)),
			subsections(bodies[i]), nil, prev, next); err != nil {
			return err
		}
	}
	return nil
}

// rewriteAnchors points `href="#x"` at the page that owns heading x when
// that page isn't the current one (base is the prefix between pages).
func rewriteAnchors(s, currentSlug, base string, anchorSection map[string]string) string {
	return anchorHrefRe.ReplaceAllStringFunc(s, func(m string) string {
		x := anchorHrefRe.FindStringSubmatch(m)[1]
		target, ok := anchorSection[x]
		if !ok || target == currentSlug {
			return m
		}
		return `href="` + base + target + `/#` + x + `"`
	})
}

// subsections lists a section's h3s for the sidebar's on-this-page part.
func subsections(body string) []tocItem {
	var out []tocItem
	for _, m := range h3TextRe.FindAllStringSubmatch(body, -1) {
		text := html.UnescapeString(strings.TrimSpace(tagRe.ReplaceAllString(m[2], "")))
		out = append(out, tocItem{Text: text, Href: "#" + m[1]})
	}
	return out
}

func renderManual(src []byte) (string, error) {
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	var buf bytes.Buffer
	if err := md.Convert(src, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// splitSections cuts the rendered manual into the pre-section intro and
// one body per h2 section.
func splitSections(rendered string) (string, []head, []string) {
	locs := h2Re.FindAllStringSubmatchIndex(rendered, -1)
	if len(locs) == 0 {
		return rendered, nil, nil
	}
	intro := rendered[:locs[0][0]]
	var heads []head
	var bodies []string
	for i, m := range locs {
		end := len(rendered)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		title := html.UnescapeString(strings.TrimSpace(tagRe.ReplaceAllString(rendered[m[4]:m[5]], "")))
		title = numberPrefixRe.ReplaceAllString(title, "")
		heads = append(heads, head{id: rendered[m[2]:m[3]], title: title})
		bodies = append(bodies, rendered[m[0]:end])
	}
	return intro, heads, bodies
}

func slugify(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

var (
	h2Re           = regexp.MustCompile(`<h2 id="([^"]+)">(.*?)</h2>`)
	h3TextRe       = regexp.MustCompile(`(?s)<h3 id="([^"]+)">(.*?)</h3>`)
	sectionIDRe    = regexp.MustCompile(`<h2 id="([^"]+)"`)
	anchorHrefRe   = regexp.MustCompile(`href="#([^"]+)"`)
	tagRe          = regexp.MustCompile(`</?[a-z]+[^>]*>`)
	numberPrefixRe = regexp.MustCompile(`^\d+\.\s*`)
)
