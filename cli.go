package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

const usage = `hop — short links and pastes in one small binary

server
  hop                          run the server (env: HOP_LISTEN, HOP_DB, HOP_TOKEN, HOP_LINKS_HOST, HOP_PASTE_HOST, ...)
  hop healthcheck              exit 0 if the local server answers /healthz (used by the Docker HEALTHCHECK)

client  (config: flags > env HOP_API / HOP_TOKEN > ~/.config/hop/config.json)
  hop login [--api URL] [--token TOKEN]   remember the API URL and token (prompts for what is missing)
  hop logout                   forget them
  hop whoami                   show the API URL and whether a token is set
  hop link <url> [slug] [--ttl 30d]       create a short link  -> prints the short URL
  hop paste [file] [--title T] [--lang L] [--ttl 7d]
                               create a paste from a file or stdin -> prints the URL
  hop ls [links|pastes]        list what exists
  hop rm <slug-or-id> [--link|--paste]    delete (auto-detects unless forced)
  hop open <slug-or-id> [-b]   print the public URL (-b: open it in the browser)
  hop version

Only the created URL goes to stdout, so you can  hop link https://… | pbcopy
`

var (
	version           = "dev"
	stdout  io.Writer = os.Stdout
	stderr  io.Writer = os.Stderr
	stdin   io.Reader = os.Stdin
)

const defaultAPI = "https://go.divyam.top"

// --- client config -------------------------------------------------------------

type clientConfig struct {
	API   string `json:"api"`
	Token string `json:"token"`
	// where each value came from; not saved
	apiFrom, tokenFrom string
}

func configPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "hop", "config.json")
}

func readConfigFile() clientConfig {
	var c clientConfig
	b, err := os.ReadFile(configPath())
	if err == nil {
		_ = json.Unmarshal(b, &c)
	}
	return c
}

func writeConfigFile(c clientConfig) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(clientConfig{API: c.API, Token: c.Token}, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

// resolveConfig applies the precedence flags > env > config file > default.
func resolveConfig(flagAPI, flagToken string) clientConfig {
	file := readConfigFile()
	c := clientConfig{}
	switch {
	case flagAPI != "":
		c.API, c.apiFrom = flagAPI, "flag"
	case os.Getenv("HOP_API") != "":
		c.API, c.apiFrom = os.Getenv("HOP_API"), "env"
	case file.API != "":
		c.API, c.apiFrom = file.API, "config"
	default:
		c.API, c.apiFrom = defaultAPI, "default"
	}
	switch {
	case flagToken != "":
		c.Token, c.tokenFrom = flagToken, "flag"
	case os.Getenv("HOP_TOKEN") != "":
		c.Token, c.tokenFrom = os.Getenv("HOP_TOKEN"), "env"
	case file.Token != "":
		c.Token, c.tokenFrom = file.Token, "config"
	default:
		c.tokenFrom = "none"
	}
	c.API = strings.TrimRight(c.API, "/")
	return c
}

// --- command line -------------------------------------------------------------------

// runCLI handles the client subcommands; it returns the process exit code.
func runCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, "hop", version)
		return 0
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "healthcheck":
		_, port, e := net.SplitHostPort(env("HOP_LISTEN", ":8090"))
		if e != nil {
			port = "8090"
		}
		c := &http.Client{Timeout: 2 * time.Second}
		resp, e := c.Get("http://127.0.0.1:" + port + "/healthz")
		if e != nil || resp.StatusCode != 200 {
			return 1
		}
		return 0
	case "login":
		err = cmdLogin(args[1:])
	case "logout":
		err = cmdLogout()
	case "whoami":
		err = cmdWhoami()
	case "link":
		err = cmdLink(args[1:])
	case "paste":
		err = cmdPaste(args[1:])
	case "ls", "list":
		err = cmdList(args[1:])
	case "rm", "delete":
		err = cmdRemove(args[1:])
	case "open", "url":
		err = cmdOpen(args[1:])
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(stderr, "hop:", err)
			fmt.Fprint(stderr, usage)
			return 2
		}
		fmt.Fprintln(stderr, "hop:", err)
		return 1
	}
	return 0
}

type usageError string

func (u usageError) Error() string { return string(u) }

// parseFlags lets flags appear before or after positional arguments
// (`hop link URL --ttl 7d` and `hop link --ttl 7d URL` both work).
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, usageError(err.Error())
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		rest = rest[1:]
	}
}

func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	api := fs.String("api", "", "API URL (https://go.example)")
	tok := fs.String("token", "", "bearer token")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cur := resolveConfig(*api, *tok)
	in := bufio.NewReader(stdin)
	if *api == "" {
		fmt.Fprintf(stderr, "API URL [%s]: ", cur.API)
		line, _ := in.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			cur.API = line
		}
	}
	if *tok == "" && os.Getenv("HOP_TOKEN") == "" {
		fmt.Fprint(stderr, "Token (input hidden): ")
		var t string
		if f, ok := stdin.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			b, err := term.ReadPassword(int(f.Fd()))
			fmt.Fprintln(stderr)
			if err != nil {
				return err
			}
			t = string(b)
		} else {
			line, _ := in.ReadString('\n')
			t = line
		}
		if t = strings.TrimSpace(t); t != "" {
			cur.Token = t
		}
	}
	if cur.Token == "" {
		return errors.New("no token given")
	}
	cur.API = strings.TrimRight(cur.API, "/")
	if err := writeConfigFile(cur); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "saved %s (api %s)\n", configPath(), cur.API)
	return nil
}

func cmdLogout() error {
	err := os.Remove(configPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintf(stderr, "removed %s\n", configPath())
	return nil
}

func cmdWhoami() error {
	c := resolveConfig("", "")
	tok := "not set"
	if c.Token != "" {
		tok = mask(c.Token) + " (" + c.tokenFrom + ")"
	}
	fmt.Fprintf(stdout, "api    %s (%s)\ntoken  %s\nconfig %s\n", c.API, c.apiFrom, tok, configPath())
	return nil
}

func mask(s string) string {
	if len(s) <= 6 {
		return "******"
	}
	return s[:3] + strings.Repeat("*", 6) + s[len(s)-3:]
}

func cmdLink(args []string) error {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	ttl := fs.String("ttl", "", "lifetime, e.g. 30d (default: forever)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usageError("link needs <url> [slug]")
	}
	body := map[string]string{"url": pos[0]}
	if len(pos) == 2 {
		body["slug"] = pos[1]
	}
	if *ttl != "" {
		body["ttl"] = *ttl
	}
	b, _ := json.Marshal(body)
	var out map[string]any
	if err := apiJSON("POST", "/api/links", "application/json", bytes.NewReader(b), nil, &out); err != nil {
		return err
	}
	fmt.Fprintln(stdout, out["short_url"])
	return nil
}

func cmdPaste(args []string) error {
	fs := flag.NewFlagSet("paste", flag.ContinueOnError)
	ttl := fs.String("ttl", "", "lifetime, e.g. 7d (default: server default, 30d)")
	title := fs.String("title", "", "title (default: the file name)")
	lang := fs.String("lang", "", "language hint for the HTML view (default: the file extension)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageError("paste takes at most one file")
	}
	var r io.Reader = stdin
	hdr := map[string]string{}
	if len(pos) == 1 && pos[0] != "-" {
		f, err := os.Open(pos[0])
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
		hdr["X-Title"] = filepath.Base(pos[0])
		if ext := strings.TrimPrefix(filepath.Ext(pos[0]), "."); ext != "" {
			hdr["X-Lang"] = ext
		}
	}
	if *title != "" {
		hdr["X-Title"] = *title
	}
	if *lang != "" {
		hdr["X-Lang"] = *lang
	}
	switch {
	case *ttl != "":
		hdr["X-TTL"] = *ttl
	case os.Getenv("HOP_TTL") != "":
		hdr["X-TTL"] = os.Getenv("HOP_TTL")
	}
	var out map[string]any
	if err := apiJSON("POST", "/api/pastes", "text/plain; charset=utf-8", r, hdr, &out); err != nil {
		return err
	}
	fmt.Fprintln(stdout, out["url"])
	return nil
}

type linkRow struct {
	Slug      string     `json:"slug"`
	URL       string     `json:"url"`
	ShortURL  string     `json:"short_url"`
	Hits      int64      `json:"hits"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type pasteRow struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Size      int64      `json:"size"`
	URL       string     `json:"url"`
	ExpiresAt *time.Time `json:"expires_at"`
	Anon      bool       `json:"anon"`
	IP        string     `json:"ip"`
}

func fetchLinks() ([]linkRow, error) {
	var ls []linkRow
	if err := apiJSON("GET", "/api/links", "", nil, nil, &ls); err != nil {
		return nil, err
	}
	c := resolveConfig("", "")
	for i := range ls {
		if ls[i].ShortURL == "" {
			ls[i].ShortURL = c.API + "/" + ls[i].Slug
		}
	}
	sort.Slice(ls, func(i, j int) bool { return ls[i].Slug < ls[j].Slug })
	return ls, nil
}

func fetchPastes() ([]pasteRow, error) {
	var ps []pasteRow
	if err := apiJSON("GET", "/api/pastes", "", nil, nil, &ps); err != nil {
		return nil, err
	}
	return ps, nil
}

func cmdList(args []string) error {
	what := "all"
	if len(args) > 0 {
		what = args[0]
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	defer tw.Flush()
	if what == "all" || what == "links" || what == "link" {
		ls, err := fetchLinks()
		if err != nil {
			return err
		}
		fmt.Fprintln(tw, "SLUG\tHITS\tEXPIRES\tTARGET")
		for _, l := range ls {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", l.Slug, l.Hits, expires(l.ExpiresAt), l.URL)
		}
		if len(ls) == 0 {
			fmt.Fprintln(tw, "(no links)")
		}
	}
	if what == "all" {
		fmt.Fprintln(tw)
	}
	if what == "all" || what == "pastes" || what == "paste" {
		ps, err := fetchPastes()
		if err != nil {
			return err
		}
		fmt.Fprintln(tw, "ID\tTITLE\tSIZE\tEXPIRES\tANON\tURL")
		for _, p := range ps {
			anon := "-"
			if p.Anon {
				anon = "anon"
				if p.IP != "" {
					anon += " " + p.IP
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\n", p.ID, p.Title, p.Size, expires(p.ExpiresAt), anon, p.URL)
		}
		if len(ps) == 0 {
			fmt.Fprintln(tw, "(no pastes)")
		}
	}
	if what != "all" && what != "links" && what != "link" && what != "pastes" && what != "paste" {
		return usageError("ls takes links or pastes")
	}
	return nil
}

func expires(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Until(*t)
	switch {
	case d <= 0:
		return "expired"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	onlyLink := fs.Bool("link", false, "treat the argument as a link slug")
	onlyPaste := fs.Bool("paste", false, "treat the argument as a paste id")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("rm needs exactly one <slug-or-id>")
	}
	id := pos[0]
	try := func(kind, path string) (bool, error) {
		code, _, err := apiDo("DELETE", path, "", nil, nil)
		if err != nil {
			return false, err
		}
		switch code {
		case http.StatusNoContent, http.StatusOK:
			fmt.Fprintf(stderr, "deleted %s %s\n", kind, id)
			return true, nil
		case http.StatusNotFound:
			return false, nil
		default:
			return false, fmt.Errorf("%s: unexpected status %d", path, code)
		}
	}
	if !*onlyPaste {
		if ok, err := try("link", "/api/links/"+id); err != nil || ok {
			return err
		}
		if *onlyLink {
			return fmt.Errorf("no such link %q", id)
		}
	}
	if ok, err := try("paste", "/api/pastes/"+id); err != nil || ok {
		return err
	}
	if *onlyPaste {
		return fmt.Errorf("no such paste %q", id)
	}
	return fmt.Errorf("no link or paste named %q", id)
}

func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	browse := fs.Bool("b", false, "open the URL in the browser")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usageError("open needs exactly one <slug-or-id>")
	}
	id := pos[0]
	var u string
	ls, err := fetchLinks()
	if err != nil {
		return err
	}
	for _, l := range ls {
		if l.Slug == id {
			u = l.ShortURL
		}
	}
	if u == "" {
		ps, err := fetchPastes()
		if err != nil {
			return err
		}
		for _, p := range ps {
			if p.ID == id {
				u = p.URL
			}
		}
	}
	if u == "" {
		return fmt.Errorf("no link or paste named %q", id)
	}
	fmt.Fprintln(stdout, u)
	if *browse {
		return browser(u)
	}
	return nil
}

func browser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

// --- http ---------------------------------------------------------------------------

// apiDo performs an authenticated request and returns the status and body.
func apiDo(method, path, ctype string, body io.Reader, hdr map[string]string) (int, []byte, error) {
	c := resolveConfig("", "")
	if c.Token == "" {
		return 0, nil, errors.New("no token: run `hop login` (or set HOP_TOKEN)")
	}
	req, err := http.NewRequest(method, c.API+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, nil
}

// apiJSON performs the request and decodes the JSON body into out; non-2xx
// responses become errors that carry the server's message.
func apiJSON(method, path, ctype string, body io.Reader, hdr map[string]string, out any) error {
	code, b, err := apiDo(method, path, ctype, body, hdr)
	if err != nil {
		return err
	}
	if code >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(b))
		}
		if code == http.StatusUnauthorized {
			return fmt.Errorf("%s: unauthorized — token rejected (run `hop login` again)", path)
		}
		return fmt.Errorf("%s: %s (%d)", path, e.Error, code)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: bad response (%d)", path, code)
	}
	return nil
}
