package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const usage = `hop — short links and pastes in one small binary

  hop                      run the server (env: HOP_LISTEN, HOP_DB, HOP_TOKEN, HOP_LINKS_HOST, HOP_PASTE_HOST, ...)
  hop link <url> [slug]    create a short link        (env: HOP_API, HOP_TOKEN)
  hop paste [file]         create a paste from a file or stdin
  hop healthcheck          exit 0 if the local server answers /healthz (used by the Docker HEALTHCHECK)
  hop version
`

var version = "dev"

// runCLI handles the client subcommands; it returns the process exit code.
func runCLI(args []string) int {
	switch args[0] {
	case "version":
		fmt.Println("hop", version)
		return 0
	case "-h", "--help", "help":
		fmt.Print(usage)
		return 0
	case "healthcheck":
		_, port, err := net.SplitHostPort(env("HOP_LISTEN", ":8090"))
		if err != nil {
			port = "8090"
		}
		c := &http.Client{Timeout: 2 * time.Second}
		resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			return 1
		}
		return 0
	case "link":
		if len(args) < 2 {
			fmt.Fprint(os.Stderr, usage)
			return 2
		}
		body := map[string]string{"url": args[1]}
		if len(args) > 2 {
			body["slug"] = args[2]
		}
		b, _ := json.Marshal(body)
		out, err := apiCall("POST", "/api/links", "application/json", bytes.NewReader(b), nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hop:", err)
			return 1
		}
		fmt.Println(out["short_url"])
		return 0
	case "paste":
		var r io.Reader = os.Stdin
		hdr := map[string]string{}
		if len(args) > 1 && args[1] != "-" {
			f, err := os.Open(args[1])
			if err != nil {
				fmt.Fprintln(os.Stderr, "hop:", err)
				return 1
			}
			defer f.Close()
			r = f
			hdr["X-Title"] = filepath.Base(args[1])
			if ext := strings.TrimPrefix(filepath.Ext(args[1]), "."); ext != "" {
				hdr["X-Lang"] = ext
			}
		}
		if v := os.Getenv("HOP_TTL"); v != "" {
			hdr["X-TTL"] = v
		}
		out, err := apiCall("POST", "/api/pastes", "text/plain; charset=utf-8", r, hdr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "hop:", err)
			return 1
		}
		fmt.Println(out["url"])
		return 0
	}
	fmt.Fprint(os.Stderr, usage)
	return 2
}

func apiCall(method, path, ctype string, body io.Reader, hdr map[string]string) (map[string]any, error) {
	api := strings.TrimRight(env("HOP_API", "https://go.divyam.top"), "/")
	tok := os.Getenv("HOP_TOKEN")
	if tok == "" {
		return nil, fmt.Errorf("HOP_TOKEN is not set")
	}
	req, err := http.NewRequest(method, api+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", ctype)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: bad response (%d)", path, resp.StatusCode)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: %v (%d)", path, out["error"], resp.StatusCode)
	}
	return out, nil
}
