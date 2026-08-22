# hop

Short links and pastes in one small Go binary. SQLite for storage, a bearer
token for writes, a two-command CLI, no JavaScript anywhere.

```
https://go.example/k8s          -> 302 to wherever you pointed it
https://paste.example/8hZp2Kq3  -> the paste as text/plain (HTML view in a browser)
```

I wrote it because I wanted a URL shortener and a paste bin that I actually
own, that fit in ~30 MB of RAM next to everything else on a small VPS, and
that I could drive from a terminal. It is deployed at `go.divyam.top` /
`paste.divyam.top` from a GitOps repo; this repo is the app.

## Quick start

```bash
docker build -t hop .
docker run -d --name hop -p 8090:8090 -v hop-data:/data \
  -e HOP_TOKEN=change-me -e HOP_LINKS_HOST=go.example -e HOP_PASTE_HOST=paste.example hop

# a short link
curl -sS -H "Authorization: Bearer change-me" -H "Content-Type: application/json" \
  -d '{"url":"https://kubernetes.io/docs/","slug":"k8s"}' http://localhost:8090/api/links
# a paste
curl -sS -H "Authorization: Bearer change-me" -H "X-Title: notes.md" --data-binary @notes.md \
  http://localhost:8090/api/pastes
```

Put it behind a reverse proxy that terminates TLS and sends the two hostnames
to it (Caddy, nginx, Traefik — one upstream, two hosts). `hop` routes by the
`Host` header.

## CLI

The same binary is the client. Point it at your instance once, then forget about tokens.

**Install** — `go install github.com/nihaldivyam/hop@latest` (needs Go), or `make build` in a
clone, or `make hop-setup` from the [voyager](https://github.com/nihaldivyam/voyager) repo which
builds it and logs in for you.

**Log in once** (writes `~/.config/hop/config.json`, mode 600):

```bash
hop login --api https://go.example        # prompts for the token, input hidden
hop whoami                                 # api + masked token + config path
```

Config precedence is **flags > `HOP_API`/`HOP_TOKEN` env > config file**, so CI can still pass the
token by env and one-offs can override with `--api/--token`.

```bash
hop link https://kubernetes.io/docs/ k8s   # -> https://go.example/k8s
hop link https://example.com --ttl 30d     # random slug, expires in 30 days
hop paste deploy.sh                         # title + language from the file name
kubectl get pods -A | hop paste --lang text # from stdin
hop link https://example.com | pbcopy       # only the URL is printed, so this just works
```

| command | what |
|---|---|
| `hop login [--api URL] [--token T]` | remember the API URL and token (prompts for what's missing) |
| `hop logout` / `hop whoami` | forget them / show them (token masked) |
| `hop link <url> [slug] [--ttl 30d]` | create a short link → prints the short URL |
| `hop paste [file] [--title T] [--lang L] [--ttl 7d]` | paste a file or stdin → prints the URL |
| `hop ls [links\|pastes]` | list what exists (slug/hits/expiry/target, id/title/size) |
| `hop rm <slug-or-id> [--link\|--paste]` | delete (auto-detects) |
| `hop open <slug-or-id> [-b]` | print the public URL (`-b` opens it in the browser) |
| `hop version` | build version |

Errors go to stderr with a non-zero exit; only the created URL goes to stdout.

## HTTP API

All writes need `Authorization: Bearer $HOP_TOKEN`. Reads are anonymous.

| method | path | body / headers | result |
|---|---|---|---|
| `POST` | `/api/links` | `{"url", "slug"?, "ttl"?}` | `201 {slug, short_url, url, expires_at}` — `409` if the slug is taken |
| `GET` | `/api/links` | | list (non-expired) |
| `DELETE` | `/api/links/{slug}` | | `204` |
| `POST` | `/api/pastes` | raw body (`text/*`, `application/octet-stream`) or multipart `file`/`content`; `X-Title`, `X-Lang`, `X-TTL` | `201 {id, url, raw_url, expires_at, size}` — `413` over the limit |
| `GET` | `/api/pastes` | | list without content |
| `DELETE` | `/api/pastes/{id}` | | `204` |
| `GET` | `/healthz` | | `200 ok` if the DB answers |

Public side — links host: `GET /{slug}` → `302`, hits and last-use recorded.
Pastes host: `GET /{id}` → `text/plain`; `GET /{id}.{ext}`, `?html=1` or a
browser `Accept: text/html` → a dark, numbered HTML view; `GET /{id}/raw` →
always plain text. `GET /` on either host is a one-paragraph landing page.

TTLs are Go durations plus `d` / `w` (`90m`, `36h`, `7d`, `2w`); `0` = never.
Expired rows stop being served immediately and are deleted by a janitor every
10 minutes.

## Configuration

| variable | default | meaning |
|---|---|---|
| `HOP_LISTEN` | `:8090` | listen address |
| `HOP_DB` | `/data/hop.db` | SQLite file (WAL mode) |
| `HOP_TOKEN` | *(empty — writes disabled)* | bearer token for the write API |
| `HOP_LINKS_HOST` | `go.divyam.top` | host that serves short links |
| `HOP_PASTE_HOST` | `paste.divyam.top` | host that serves pastes |
| `HOP_MAX_PASTE_BYTES` | `262144` | paste size limit |
| `HOP_DEFAULT_PASTE_TTL` | `30d` | default paste lifetime (`0` = forever) |
| `HOP_REPO_URL` | this repo | link shown on the landing pages |
| `HOP_TRUST_PROXY` | `true` | use `X-Forwarded-For` for the per-IP rate limit |

## Security notes

- Writes need the token; it is compared in constant time. No token → the write
  API answers `503` instead of being open.
- Request bodies are capped (1 MiB for JSON, `HOP_MAX_PASTE_BYTES` for pastes).
- Pastes are served as `text/plain` unless the client *asks* for HTML, and the
  HTML view escapes everything and sets `Content-Security-Policy: default-src
  'none'; style-src 'self'` — a paste can never run as a page.
- Anonymous reads are lightly rate-limited per client IP (10 req/s, burst 30).
  Behind a reverse proxy keep `HOP_TRUST_PROXY=true`; running it naked on the
  internet, set it to `false` so the header can't be spoofed.
- Slugs are `[A-Za-z0-9_-]{1,64}`; `api`, `healthz`, `static`, `raw`, … are reserved.
- The image is distroless, runs as `nonroot`, and only `/data` is writable.

## Development

```bash
make test      # go vet + go test
make run       # local server on :8090, token "dev", ./hop.db
make docker    # build the image
```

MIT — see LICENSE.
