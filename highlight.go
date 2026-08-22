package main

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"time"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Highlighting is server-side (chroma) and class-based, so the paste view keeps
// its strict CSP: no inline styles, no scripts. Everything past these caps is
// rendered with the plain-text lexer — still numbered and linkable, just not
// coloured, and never run through lexers.Analyse.
const (
	maxHighlightBytes = 200 << 10
	maxHighlightLines = 5000
	highlightStyle    = "github-dark"
)

var (
	codeFormatter = html.New(
		html.WithClasses(true),
		html.WithLineNumbers(true),
		html.WithLinkableLineNumbers(true, "L"),
		html.TabWidth(4),
	)
	codeStyle = func() *chroma.Style {
		if s := styles.Get(highlightStyle); s != nil {
			return s
		}
		return styles.Fallback
	}()
)

// pickLexer chooses by explicit language (X-Lang or the URL extension), then
// by the paste's title (a filename), then by sniffing — unless the paste is
// too big to bother, in which case it is plain text.
func pickLexer(lang, title string, content []byte) chroma.Lexer {
	tooBig := len(content) > maxHighlightBytes || bytes.Count(content, []byte{'\n'}) > maxHighlightLines
	var l chroma.Lexer
	if lang = strings.ToLower(strings.TrimSpace(lang)); lang != "" {
		l = lexers.Get(lang)
		if l == nil {
			l = lexers.Match("f." + lang)
		}
	}
	if l == nil && title != "" {
		l = lexers.Match(title)
	}
	if l == nil && !tooBig {
		l = lexers.Analyse(string(content))
	}
	if l == nil || tooBig {
		l = lexers.Fallback
	}
	return chroma.Coalesce(l)
}

// langName is what the view shows as the language tag.
func langName(l chroma.Lexer) string {
	if l == nil || l.Config() == nil {
		return "text"
	}
	if n := l.Config().Name; n != "" && n != lexers.Fallback.Config().Name {
		return n
	}
	return "text"
}

// renderCode returns the highlighted, numbered HTML for one paste.
func renderCode(l chroma.Lexer, content []byte) (template.HTML, error) {
	it, err := l.Tokenise(nil, string(content))
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := codeFormatter.Format(&buf, codeStyle, it); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// highlightCSS is the chroma stylesheet for the chosen style, generated once.
func highlightCSS() []byte {
	var buf bytes.Buffer
	if err := codeFormatter.WriteCSS(&buf, codeStyle); err != nil {
		return nil
	}
	// the page supplies its own background; let the code block sit on it
	buf.WriteString("\n.chroma { background-color: transparent; }\n")
	return buf.Bytes()
}

// --- small presentation helpers ---------------------------------------------

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
var extLike = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// downloadName builds a safe attachment filename: the title if there is one,
// else the id, with an extension from the title, the language, or .txt.
func downloadName(p *Paste, lang string) string {
	base := strings.TrimLeft(unsafeName.ReplaceAllString(strings.TrimSpace(p.Title), "_"), "._")
	if base == "" {
		base = p.ID
	}
	if !strings.Contains(base, ".") {
		if l := strings.ToLower(lang); extLike.MatchString(l) {
			base += "." + l
		} else {
			base += ".txt"
		}
	}
	return base
}

// humanDur is a coarse "in 29d" / "in 3h" / "in 12m" for expiry labels.
func humanDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
