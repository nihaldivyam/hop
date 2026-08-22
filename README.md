# hop

Short links and pastes in one small Go binary. SQLite for storage, a bearer
token for writes, a two-command CLI, a tiny dependency-free UI on the two
landing pages, and no JavaScript in the paste view.

```
https://go.example/k8s          -> 302 to wherever you pointed it
https://paste.example/8hZp2Kq3  -> the paste as text/plain (HTML view in a browser)
```

I wrote it because I wanted a URL shortener and a paste bin that I actually
own, that fit in ~30 MB of RAM next to everything else on a small VPS, and
that I could drive from a terminal. It is deployed at `go.divyam.top` /
`paste.divyam.top` from a GitOps repo; this repo is the app.

## Using hop

Three ways, all talking to the same API. Reading a link or a paste never needs
anything; creating one needs the write token (`HOP_TOKEN`) — except when
[anonymous pastes](#anonymous-pastes) / [anonymous short links](#anonymous-short-links)
are enabled, which work without a token inside hard limits.

- **In the browser** — open `https://go.divyam.top/` or `https://paste.divyam.top/`,
  paste the token once (it stays in that browser's `localStorage` and is only sent
  to that origin), then create links / pastes, copy URLs, see your list, delete.
- **From a terminal** — `hop link <url> [slug]`, `… | hop paste`, `hop paste file`
  (see [CLI](#cli)), or two `curl` calls (see [HTTP API](#http-api)).
- **From the GitOps repo that deploys it** — `make short URL=… [SLUG=…]` and
  `make paste FILE=…` print the URL to share; the token is read over ssh from the
  box (`/opt/voyager/secrets/hop.env`).

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
| `hop paste [file] [--title T] [--lang L] [--ttl 7d] [--id NAME]` | paste a file or stdin → prints the URL (`--id`: name your own URL) |
| `hop ls [links\|pastes]` | list what exists (slug/hits/expiry/target, id/title/size) |
| `hop rm <slug-or-id> [--link\|--paste]` | delete (auto-detects) |
| `hop open <slug-or-id> [-b]` | print the public URL (`-b` opens it in the browser) |
| `hop version` | build version |

Errors go to stderr with a non-zero exit; only the created URL goes to stdout.

## Accounts (sign in with Zitadel)

Set `OIDC_CLIENT_ID` (+ `OIDC_ISSUER`) and hop grows user accounts on both hosts:
**Sign in / Sign up** in the header (an OIDC authorization-code + PKCE flow against
the identity provider — Zitadel at `accounts.divyam.top` in production, which also
handles registration, email verification, passkeys and MFA), an encrypted session
cookie, **your own links and pastes** (created while signed in they carry your
subject; the lists, deletes and `hop ls` show only yours), **per-user API tokens**
(`/account` → create; `hop login --token hop_u_…`; `Authorization: Bearer hop_u_…`
— shown once, stored hashed, revocable, act as you and respect your plan) and
**plan-based limits** fetched from the billing service. With `OIDC_CLIENT_ID` empty
nothing changes — owner token and anonymous modes work exactly as before.

Routes (both hosts): `GET /login` (`?next=/path`), `GET /auth/callback`, `GET /logout`
(RP-initiated logout at the IdP), `GET /me` (JSON), `GET /account` (HTML), `GET|POST
/api/tokens`, `DELETE /api/tokens/{id}`. Cookie-authenticated writes from the browser
must carry `X-Requested-With: hop` (the UI does); API tokens never need it.

| plan | pastes up to | lifetime | links | rate | items |
|---|---|---|---|---|---|
| anonymous | HOP_ANON_* caps | ≤ 24 h / 7 d | random slug, confirmation page | HOP_ANON_*_RATE | — |
| **free** (signed in) | 256 KiB | ≤ 30 d | custom slugs, direct redirect, ≤ 30 d | 30 / h | 500 |
| **pro** | 1 MiB | forever allowed | forever allowed | 300 / h | 10 000 |
| owner token | instance limits | any | any | none | — |

Env: `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` (optional), `OIDC_REDIRECT_URLS`
(comma list, one `…/auth/callback` per host; the one matching the request host is used),
`HOP_SESSION_SECRET` (random-and-logged if empty — set it, or sessions die on restart),
`HOP_COOKIE_DOMAIN` (e.g. `divyam.top`: one sign-in for both hosts), `BILLING_URL` +
`BILLING_INTERNAL_TOKEN` (`GET /internal/entitlements/{sub}` → `{"plan":"free"|"pro"}`,
cached 5 min, "free" on error/unset), `HOP_BILLING_ACCOUNT_URL` (the upgrade link).
Discovery is retried in the background, so a slow IdP never blocks links and pastes.

## Paste view

Opening a paste in a browser (`/{id}`, `/{id}.{ext}`, or `?html=1`) renders it
server-side with [chroma](https://github.com/alecthomas/chroma): the language
comes from `X-Lang`, the URL extension, the title's extension, or sniffing;
lines are numbered and linkable (`#L12` highlights a line), there is a CSS-only
**Wrap** toggle, **Raw** (`/{id}/raw`) and **Download** (`/{id}/raw?dl=1`, a
safe filename derived from the title/language), and a collapsible plain-text box
for select-all/copy. Pastes over 200 KiB or 5 000 lines are shown unhighlighted.
The view stays under the strict CSP (`default-src 'none'; style-src 'self'` —
class-based highlighting, no inline styles, no scripts); its stylesheet is
`/static/paste.css`. Markdown pastes are highlighted as Markdown source, not
rendered (no HTML passthrough, so nothing to sanitise).

## HTTP API

All writes need `Authorization: Bearer $HOP_TOKEN` (the exceptions are
`POST /api/pastes` / `POST /api/links` without any `Authorization` header when
`HOP_PUBLIC_PASTES=true` / `HOP_PUBLIC_LINKS=true`, see [Anonymous pastes](#anonymous-pastes)
and [Anonymous short links](#anonymous-short-links)). Reads are anonymous.

| method | path | body / headers | result |
|---|---|---|---|
| `POST` | `/api/links` | `{"url", "slug"?, "ttl"?}` | `201 {slug, short_url, url, expires_at, anon}` — `409` if the slug is taken |
| `GET` | `/api/links` | | list (non-expired; `anon`, and `ip` for anonymous links) |
| `DELETE` | `/api/links/{slug}` | | `204` |
| `DELETE` | `/api/links?anon=1` | | `200 {deleted}` — purge every anonymous link |
| `POST` | `/api/pastes` | raw body (`text/*`, `application/octet-stream`) or multipart `file`/`content`; `X-Title`, `X-Lang`, `X-TTL`, optional `X-Id` (or `?id=`) to name the URL | `201 {id, url, raw_url, expires_at, size}` — `413` over the limit, `400` invalid id, `409` id taken |
| `GET` | `/api/pastes` | | list without content (`anon`, and `ip` for anonymous pastes) |
| `DELETE` | `/api/pastes/{id}` | | `204` |
| `DELETE` | `/api/pastes?anon=1` | | `200 {deleted}` — purge every anonymous paste |
| `GET` | `/healthz` | | `200 ok` if the DB answers |

Public side — links host: `GET /{slug}` → `302`, hits and last-use recorded
(anonymous links: browsers get a `200` confirmation page first, `GET /{slug}/go`
does the redirect — see [Anonymous short links](#anonymous-short-links)).
Pastes host: `GET /{id}` → `text/plain`; `GET /{id}.{ext}`, `?html=1` or a
browser `Accept: text/html` → a dark, numbered HTML view; `GET /{id}/raw` →
always plain text. A browser opening a *free* valid name (`GET /{name}`,
`Accept: text/html`) gets a `200` editor that creates the paste at that name —
see [Name your own paste URL](#name-your-own-paste-url); other clients get `404`. `GET /` on either host is a one-paragraph landing page.

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
| `OIDC_ISSUER` / `OIDC_CLIENT_ID` / `OIDC_CLIENT_SECRET` | *(empty — accounts off)* | sign in through an OIDC provider (see Accounts) |
| `OIDC_REDIRECT_URLS` | *(derived)* | comma list of `https://<host>/auth/callback` |
| `HOP_SESSION_SECRET` | *(random)* | encrypts the session cookie |
| `HOP_COOKIE_DOMAIN` | *(host only)* | share the session across hosts, e.g. `divyam.top` |
| `BILLING_URL` / `BILLING_INTERNAL_TOKEN` | *(empty — everyone is "free")* | plan lookups |
| `HOP_BILLING_ACCOUNT_URL` | `https://billing.divyam.top/account` | upgrade link on `/account` |
| `HOP_PUBLIC_PASTES` | `false` | accept anonymous pastes (paste host only, see below) |
| `HOP_ANON_MAX_BYTES` | `32768` | size cap for anonymous pastes (→ `413`) |
| `HOP_ANON_MAX_TTL` | `24h` | anonymous pastes always expire; longer/`0` requests are clamped |
| `HOP_ANON_RATE` | `5/1h` | anonymous creates per client IP (`N/period`) |
| `HOP_ANON_BURST` | `2` | burst allowed on top of `HOP_ANON_RATE` |
| `HOP_ANON_DAILY_CAP` | `200` | global anonymous pastes per UTC day (→ `429 daily cap reached`) |
| `HOP_PUBLIC_LINKS` | `false` | accept anonymous short links (links host only, see below) |
| `HOP_ANON_LINK_MAX_TTL` | `7d` | anonymous links always expire; longer/`0` requests are clamped |
| `HOP_ANON_LINK_RATE` | `5/1h` | anonymous link creates per client IP (`N/period`; separate bucket from pastes) |
| `HOP_ANON_LINK_BURST` | `2` | burst allowed on top of `HOP_ANON_LINK_RATE` |
| `HOP_ANON_LINK_DAILY_CAP` | `200` | global anonymous links per UTC day (→ `429 daily cap reached`) |
| `HOP_ANON_LINK_INTERSTITIAL` | `true` | browsers see a confirmation page before an anonymous redirect |

## Name your own paste URL

Any free paste URL can be claimed directly: open `https://paste.divyam.top/<name>`
in a browser and, if no paste lives there yet and the name is valid, you get an
editor that saves **at exactly that URL** (the page POSTs to `/api/pastes` with
`X-Id: <name>` and then navigates to the new paste). Names are **1–15 characters**,
letters/digits/`-`/`_`, starting with a letter or digit, and must not be a reserved
path (`api`, `raw`, `static`, `healthz`, `go`, `admin`, …); a taken name is `409`,
an invalid one is a `404` page that states the rule. Other ways to name a paste:
the **Custom URL** field on the landing page, `hop paste file --id my-notes`, or
`curl -H "X-Id: my-notes" --data-binary @file https://paste.divyam.top/api/pastes`
(`?id=my-notes` works too). Anonymous users may pick names under the usual anonymous
limits (24 h expiry, size, rate limit), so squatted names free themselves; the token
gives the full limits. Random ids stay 8-char base58. Names are not reserved for
anyone — first come, first served until the paste expires.

## Anonymous pastes

Off by default. With `HOP_PUBLIC_PASTES=true` the paste host's landing page
shows the form straight away and `POST /api/pastes` without an `Authorization`
header creates an *anonymous* paste — short links never work without the token
(an open redirector would lend the domain to phishing). Guard rails, all
configurable (table above):

- size ≤ `HOP_ANON_MAX_BYTES` (32 KiB) → otherwise `413` naming the limit;
- lifetime ≤ `HOP_ANON_MAX_TTL` (24 h): a requested TTL above it, or `0`/forever,
  is silently clamped — `expires_at` in the response is the truth;
- text only: bodies containing NUL bytes get `415`;
- per-IP token bucket `HOP_ANON_RATE` / `HOP_ANON_BURST` → `429 {"error":"rate
  limited","retry_after_seconds":N}` plus a `Retry-After` header;
- a global `HOP_ANON_DAILY_CAP` per UTC day → `429 {"error":"daily cap reached"}`;
- the creator IP is stored with anonymous pastes (only visible to token holders
  via `GET /api/pastes` and `hop ls pastes`, never on the public view), the view
  carries an "anonymous paste · report abuse" line and stays `noindex`;
- a wrong token is still `401` — it never downgrades to anonymous.

Handling abuse: `hop ls pastes` shows `anon <ip>`, `hop rm <id>` removes one,
`DELETE /api/pastes?anon=1` (token) purges all anonymous pastes at once, and
`HOP_PUBLIC_PASTES=false` + restart is the kill switch. Behind CrowdSec the 429s
in the access log also feed its ban decisions for floods.

## Anonymous short links

Off by default — an open redirector lends the domain to phishing, so this is
deliberately narrower than anonymous pastes. With `HOP_PUBLIC_LINKS=true` the
links host's landing page shows the form straight away and `POST /api/links`
without an `Authorization` header creates an *anonymous* link:

- **random slug only** (6 chars, one longer than token slugs); a `slug` in the
  request is `400` — custom slugs need the token;
- destination must be absolute http(s), ≤ 2048 chars, without credentials
  (`user:pass@`), and must not point at `localhost`, private/loopback/link-local
  IP literals, or hop itself → `400`;
- lifetime ≤ `HOP_ANON_LINK_MAX_TTL` (7 d): longer or `0` is clamped —
  `expires_at` in the response is the truth;
- per-IP token bucket `HOP_ANON_LINK_RATE` / `HOP_ANON_LINK_BURST` (its own
  bucket, separate from pastes) → `429 {"error":"rate limited","retry_after_seconds":N}`
  with `Retry-After`; a global `HOP_ANON_LINK_DAILY_CAP` per UTC day → `429 {"error":"daily cap reached"}`;
- **confirmation page**: with `HOP_ANON_LINK_INTERSTITIAL=true` (default) a
  browser opening `/{slug}` gets a `200` page that shows the full destination
  (host in bold), "created … ago · expires …", a *report abuse* link, and a
  **Continue →** button to `/{slug}/go`, which performs the `302` (and counts
  the hit). The page is `noindex`, `no-referrer`, `no-store`, no scripts, strict
  CSP. Non-browser clients (`curl`, no `Accept: text/html`) get the `302`
  directly — people are the concern, not scanners. Token-created links always
  redirect directly;
- the creator IP is stored with anonymous links (visible to token holders via
  `GET /api/links` and `hop ls links`, never publicly); a wrong token is still
  `401`, it never downgrades to anonymous.

Handling abuse: `hop ls links` shows `anon <ip>`, `hop rm <slug>` removes one,
`DELETE /api/links?anon=1` (token) purges all anonymous links at once, and
`HOP_PUBLIC_LINKS=false` + restart is the kill switch (or set
`HOP_ANON_LINK_INTERSTITIAL=false` to drop only the confirmation page).

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
