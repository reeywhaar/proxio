# proxio

An HTTP relay in a 7 MB container. Hand it a token and an encoded URL, and it makes that
request for you — method, headers and body intact, streamed both ways.

```
caller ──GET /proxy?token=px_…&url=https%3A%2F%2Fapi.example.com%2Fthing──▶ proxio ──▶ api.example.com
       ◀────────────────────── streamed back, headers intact ───────────────
```

No proxy protocol to configure, no client library, no `HTTP_PROXY` to export, and no header to
set. An ordinary request to an ordinary URL is the whole interface, which is what makes it
usable from a shell script, a cron job, a `<video>` tag, or a config file with nowhere to put
a header — **and what lets one proxio stand in front of another**.

## Contents

- [Read this before deploying](#read-this-before-deploying) — the one thing that will bite you
- [Setup](#setup) — [1. Run it](#1-run-it) · [2. Create a token](#2-create-a-token) · [3. Make a request](#3-make-a-request)
- [Usage](#usage) — the endpoint, both headers, streaming
- [Command line](#command-line)
- [Configuration](#configuration)
- [How it works](#how-it-works)
- [What it deliberately does not do](#what-it-deliberately-does-not-do)
- [Development](#development)
- Full reference: [docs/](docs/) — [proxying](docs/proxying.md) · [tokens](docs/tokens.md) · [deploy](docs/deploy.md)

## Read this before deploying

**A token is proxio's whole security boundary.** proxio fetches any URL it is given, including
addresses only proxio can reach — other containers on its networks, services bound to the
host, and cloud metadata endpoints such as `169.254.169.254`, which on most providers hand out
credentials to anyone who asks. There is no destination filtering and no flag that adds one.

Anyone holding a token has proxio's network reach. That is deliberate — reaching internal
services is often the reason to run it — and it makes two things load-bearing:

- **Put proxio on a network with nothing on it that a token-holder should not reach.**
- **Give each service its own labelled token**, so a leaked one can be withdrawn on its own.

Two smaller ones, so they are not surprises:

- **`/data` must be mounted.** proxio refuses to start without it, rather than keeping your
  tokens somewhere that disappears with the container.
- **The token travels in the URL**, so whatever sits in front of proxio logs it. proxio's own
  logs never carry it — it drops the query string from every URL it writes down — but caddy's
  access log records the full request URI. Filter it or turn it off. The same goes for browser
  history and anywhere a URL gets pasted.

## Setup

### 1. Run it

**proxio listens on `:80` inside the container**, always. Behind a reverse proxy that means
nothing needs publishing to the host at all; on its own, remap it with `-p`.

With compose, behind [caddy-docker-proxy](https://github.com/lucaslorentz/caddy-docker-proxy):

```yaml
services:
  proxio:
    image: ghcr.io/reeywhaar/proxio:latest
    restart: unless-stopped

    volumes:
      - data:/data              # required: proxio will not start without it

    # The image already declares this, so compose inherits it either way — spelled out
    # here because a healthcheck you cannot see in the compose file is one nobody knows
    # they have.
    healthcheck:
      test: ["CMD", "proxio", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3

    # Nothing is published to the host; caddy reaches proxio over this network.
    # Anything else on it is reachable by anyone holding a token, so keep it short.
    networks:
      - caddy

    labels:
      caddy: proxio.example.com
      caddy.reverse_proxy: "{{upstreams 80}}"
      caddy.log.output: stdout
      caddy.log.format: json

volumes:
  data:

networks:
  caddy:
    external: true
```

```sh
docker compose up -d
```

caddy gets the certificate and terminates TLS; proxio speaks plain HTTP behind it and never
learns its own hostname — there is no public-URL setting to keep in sync, so moving it to a
different domain is a label change and nothing else.

[`docker-compose.yml`](docker-compose.yml) is that file with every setting present and
commented out at its default, so there is one file to read rather than a compose file that
points at an env file holding the explanations.

Or directly, publishing a port for local use:

```sh
docker run -d --name proxio \
  -v proxio-data:/data \
  -p 8080:80 \
  ghcr.io/reeywhaar/proxio:latest
```

The `-v` is the one flag you cannot omit.

### 2. Create a token

```sh
TOKEN=$(docker exec proxio proxio token create some_service)
```

**Shown once.** It is stored hashed, so a lost token is deleted and minted again rather than
recovered. The doubled `proxio proxio` is not a typo: `docker exec` bypasses the image
`ENTRYPOINT`, so the binary has to be named.

The running server picks it up on the next request — there is nothing to restart.

### 3. Make a request

```sh
curl "http://localhost:8080/proxy?token=$TOKEN&url=https%3A%2F%2Fapi.github.com%2Fusers%2Foctocat"
```

That is the whole product.

## Usage

```
<any method> /proxy?url={percent-encoded absolute URL}&token={token}[&hide=1]
```

The method, the headers and the body are inherited. The status, the headers and the body come
back.

| parameter | | |
| --- | --- | --- |
| `url` | required | The absolute `http` or `https` URL to fetch, percent-encoded |
| `token` | required | Missing or unknown is a `401` |
| `hide` | optional | `hide=1` tells the target nothing about who asked |

**Any of the three given twice is a `400`** — for `token` especially, "first wins" and "last
wins" are two different access decisions from the same URL.

Percent-encoding is what keeps the target's own query string out of proxio's:

```sh
target='https://api.example.com/search?q=hello world&page=2'
curl "https://proxio.example.com/proxy?token=$TOKEN&url=$(printf %s "$target" | jq -sRr @uri)"
```

### `hide`

By default the target is told: `X-Forwarded-For` (your address appended to any inbound chain),
`X-Real-Ip`, and `Via: 1.1 proxio`. With `hide=1`, none of those go out **and any that arrived
are stripped** — so sitting behind another proxy does not leak you through the header it wrote.

`1`, `true`, `yes`, `on` turn it on; `0`, `false`, `no`, `off` turn it off. **Anything else is
a `400`, including a bare `?hide` with no value** — that reads like a flag meaning *on*, and a
parameter whose only job is concealment must not quietly mean the opposite of what it looks
like.

It does not touch `Referer`, `Origin`, `User-Agent` or `Cookie`. Those are yours to send or
not; proxio will not remove them for you.

### From a browser

The token is a parameter, not a header, so `<img>`, `<video>`, `<audio>`, `<script>` and plain
links work with no preflight at all. `fetch()` works too — proxio forwards `Origin`, relays
the target's `Access-Control-*` headers untouched, and relays preflight `OPTIONS` to the
target — **wherever the target's own CORS policy would have allowed it**. A target that sends
`Access-Control-Allow-Origin: *` is readable; one that sends nothing is blocked by the
browser, and proxio does not override that.

Remember that the token is then in a page's HTML and in the browser's history.

### Chaining

A proxio's target can be another proxio. Each hop carries its own token in its own URL, so
neither knows the other's — which is why the credential is a parameter and not a header. With
a header the first hop would have to forward its own credential for the second to see one, and
forwarding a credential to whatever host the caller named is the one thing a proxy must never
do.

```sh
inner=$(printf %s "https://b.example.com/proxy?token=$B_TOKEN&url=$(printf %s "$TARGET" | jq -sRr @uri)" | jq -sRr @uri)
curl "https://a.example.com/proxy?token=$A_TOKEN&url=$inner"
```

`hide` applies per hop: on the outer URL it hides you from B, on the inner it hides A from the
target. `Via` and `X-Forwarded-For` gain an entry per hop, so the chain reads as a chain.

### Streaming

Both directions, no buffering, no size limit. A byte the target produces reaches you without
waiting for the byte after it, so server-sent events, a log tail and a 40 GB file all work.

```sh
curl -N "https://proxio.example.com/proxy?token=$TOKEN&url=https%3A%2F%2Fexample.com%2Fevents"
```

There is no timeout on a response body. `PROXIO_RESPONSE_TIMEOUT` bounds only the wait for the
*first* byte.

### Redirects

proxio follows them for you, up to ten hops, and you get the final response.
`PROXIO_MAX_REDIRECTS=0` turns that off and hands the `3xx` back with its `Location` intact.

### Telling proxio's failures from the target's

proxio's own errors carry `X-Proxio-Error` and a JSON body; a relayed response never does. So
a `502` with the header is *a proxio could not reach its target*, and a `502` without it is
*the target said 502*. In a chain it tells you a hop failed, not which one.

```
X-Proxio-Error: upstream
{"error":"dial tcp: connection refused"}
```

## Command line

```sh
proxio serve                      # what the image runs
proxio token create <label>       # prints the secret once, on stdout
proxio token list [--json]
proxio token delete <label>
proxio healthcheck                # what HEALTHCHECK runs
proxio version
```

`token create` prints the secret alone on stdout and everything else on stderr, so
`TOKEN=$(…)` captures the token and nothing else. Labels may hold letters, digits, `_`, `-`
and `.`, and are the key a token is deleted by. → [docs/tokens.md](docs/tokens.md)

## Configuration

| variable | default | meaning |
| --- | --- | --- |
| `PROXIO_DATA_DIR` | `/data` | Where `tokens.json` lives. Must exist and be writable |
| `PROXIO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `PROXIO_DIAL_TIMEOUT` | `10s` | TCP connect and TLS handshake to the target |
| `PROXIO_RESPONSE_TIMEOUT` | `30s` | How long a target has to **begin** answering |
| `PROXIO_MAX_REDIRECTS` | `10` | `0` passes a `3xx` through untouched |

No config file, and no public URL — proxio builds no links and never needs to know its own
address, so moving it costs nothing. `:80` inside the container, remapped with `-p`.

## How it works

One Go binary. A request to `/proxy` is authenticated against a hash before anything else is
parsed, then the target is validated, the request rebuilt with the hop-by-hop headers dropped
and the forwarding headers decided, and handed to an `http.Client` carrying the caller's own
context — so hanging up cancels the fetch, the whole way down a chain. The response is copied
back a chunk at a time, flushing each one.

Of a target URL the log keeps the scheme, host and path and drops the rest, marking it
`?{redacted}`. Dropping the query whole rather than filtering it is what makes a chain safe to
log: a chained target is another proxio's `/proxy` URL, and the next hop's token and the whole
nested URL both live in its query.

Tokens live in `/data/tokens.json` as labels and SHA-256 digests. The server stats that file
before each authentication and re-reads it when it changes, which is how `docker exec proxio
proxio token create` — a *different process* — takes effect on the next request with nothing
restarted.

SHA-256 rather than bcrypt, deliberately: bcrypt makes a password a person chose expensive to
guess, and a 256-bit random token is not guessable at any cost. A slow hash would be paid for
by every proxied request and buy nothing.

The details, and the arguments behind them, are in [docs/](docs/).

## What it deliberately does not do

Each of these was considered and left out. They are here so they are not re-proposed every few
months.

- **No forward-proxy mode.** No `CONNECT`, no absolute-URI requests, no `HTTP_PROXY` support.
  One endpoint, one code path.
- **No manufactured CORS.** The target's `Access-Control-*` headers are relayed like any
  others, so `fetch()` works through proxio wherever it would work against the target
  directly. What proxio will not do is invent `Access-Control-Allow-Origin: *` to make a
  target readable from a page when the target did not say so — that is a different product,
  and it would mean intercepting `OPTIONS` instead of proxying it.
- **No WebSocket relay.** `Upgrade` is hop-by-hop and is dropped; a `ws://` target is not
  reachable.
- **No per-token host rules.** Every token has proxio's full reach. This is the most likely
  next feature, and `tokens.json` carries a version field so adding it is a migration rather
  than a break.
- **No destination filtering.** See [Read this before deploying](#read-this-before-deploying).
- **No rate limiting, quotas or usage counters.** The access log is the record.
- **No response caching.** proxio is a relay; a cache is a second product.
- **No trailers.**

## Development

```sh
go test ./...
gofmt -l .
go vet ./...

docker build -t proxio:dev .
```

`serve` binds `:80`, so it runs in the container rather than on a laptop; the tests cover the
handler directly against `httptest` targets. `go run . token create x` works anywhere with
`PROXIO_DATA_DIR` pointed somewhere writable.

Conventions — commit messages, comments, naming — are in
[docs/conventions.md](docs/conventions.md).
