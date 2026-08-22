package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPasteViewHighlighting(t *testing.T) {
	_, ts := testServer(t)
	src := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"<b>hi</b>\")\n}\n"
	r, b := do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader(src),
		map[string]string{"X-Title": "main.go", "X-Lang": "go"})
	if r.StatusCode != 201 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	var out map[string]any
	json.Unmarshal(b, &out)
	id := out["id"].(string)

	// browser view: chroma classes, token colouring, line anchors, escaped content, strict CSP, no scripts/inline styles
	r, b = do(t, ts, "GET", "paste.example", "/"+id, "", "", nil, map[string]string{"Accept": "text/html"})
	body := string(b)
	for _, want := range []string{`class="chroma"`, `class="kn">package`, `id="L1"`, `href="#L6"`, "&lt;b&gt;hi&lt;/b&gt;", `/static/paste.css`, `class="tag">Go<`, `Download`} {
		if !strings.Contains(body, want) {
			t.Errorf("view missing %q", want)
		}
	}
	for _, bad := range []string{"<script", "style=\"", "<b>hi</b>"} {
		if strings.Contains(body, bad) {
			t.Errorf("view contains %q", bad)
		}
	}
	if csp := r.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "style-src 'self'") {
		t.Errorf("csp: %q", csp)
	}
	// the .ext form overrides the stored language
	_, b = do(t, ts, "GET", "paste.example", "/"+id+".py", "", "", nil, nil)
	if !strings.Contains(string(b), `class="tag">Python<`) {
		t.Errorf("ext should pick python: %s", b)
	}
	// download
	r, b = do(t, ts, "GET", "paste.example", "/"+id+"/raw?dl=1", "", "", nil, nil)
	if cd := r.Header.Get("Content-Disposition"); cd != `attachment; filename="main.go"` || string(b) != src {
		t.Errorf("download: %q %q", cd, b)
	}
	if r, _ = do(t, ts, "GET", "paste.example", "/"+id+"/raw", "", "", nil, nil); r.Header.Get("Content-Disposition") != "inline" {
		t.Errorf("raw should be inline")
	}
	// the stylesheet is served and contains the chroma rules
	r, b = do(t, ts, "GET", "paste.example", "/static/paste.css", "", "", nil, nil)
	if r.StatusCode != 200 || !strings.Contains(string(b), ".chroma") || !strings.Contains(string(b), ".sitenav") {
		t.Errorf("paste.css: %d", r.StatusCode)
	}
}

func TestPasteViewPlainFallback(t *testing.T) {
	_, ts := testServer(t)
	// unknown language, unsniffable content → plain text lexer, still numbered
	r, b := do(t, ts, "POST", "paste.example", "/api/pastes", "secret", "text/plain", strings.NewReader("just words\nmore words\n"),
		map[string]string{"X-Lang": "nosuchlang"})
	if r.StatusCode != 201 {
		t.Fatalf("create: %d %s", r.StatusCode, b)
	}
	var out map[string]any
	json.Unmarshal(b, &out)
	id := out["id"].(string)
	_, b = do(t, ts, "GET", "paste.example", "/"+id+"?html=1", "", "", nil, nil)
	if !strings.Contains(string(b), `class="tag">text<`) || strings.Count(string(b), `class="line"`) != 2 {
		t.Errorf("plain fallback: %s", b)
	}
	// download name falls back to id.txt
	r, _ = do(t, ts, "GET", "paste.example", "/"+id+"/raw?dl=1", "", "", nil, nil)
	if want := `attachment; filename="` + id + `.txt"`; r.Header.Get("Content-Disposition") != want {
		t.Errorf("download name: %q", r.Header.Get("Content-Disposition"))
	}
}

func TestPickLexerCaps(t *testing.T) {
	big := []byte(strings.Repeat("x := 1\n", maxHighlightLines+10))
	if l := pickLexer("go", "", big); langName(l) != "text" {
		t.Errorf("oversized paste should be plain, got %s", langName(l))
	}
	if l := pickLexer("", "notes.md", []byte("# hi\n")); langName(l) != "markdown" {
		t.Errorf("title extension should pick markdown, got %s", langName(l))
	}
	if l := pickLexer("", "", []byte("#!/bin/sh\necho hi\n")); langName(l) == "text" {
		t.Errorf("sniffing should find a shell lexer")
	}
	if got := downloadName(&Paste{ID: "abc", Title: "../weird name?.sh"}, "sh"); got != "weird_name_.sh" {
		t.Errorf("downloadName sanitising: %q", got)
	}
}
