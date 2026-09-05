# Deploy

One image, one port, one volume.

```
ghcr.io/reeywhaar/proxio:latest
```

## Read this first

**The token travels in the URL**, so anything in front of proxio logs it. caddy's access log
records the full request URI; filter that parameter out of it or turn the log off. proxio's
own logs never carry a token — it writes down only the scheme, host and path of a target — but
it cannot reach into the proxy in front of it, browser history, or a URL somebody pastes into
a chat.

**A token is proxio's whole security boundary.** proxio fetches any URL it is given, including
addresses only proxio can reach: other containers on its networks, services bound to the host,
and cloud metadata endpoints such as `169.254.169.254`, which on most providers hand out
credentials to anyone who asks. There is no destination filtering and no flag to add one.

Anyone holding a token has proxio's network reach. That is a deliberate property — reaching
internal services is often the reason to run it — and it makes two things load-bearing:

- **Put proxio on a network with nothing on it a token-holder should not reach.** In the
  compose file that is one `caddy` network and no others.
- **One labelled token per service**, so a leaked one can be withdrawn on its own.

## Environment

| variable | default | meaning |
| --- | --- | --- |
| `PROXIO_DATA_DIR` | `/data` | Where `tokens.json` lives. Must exist and be writable |
| `PROXIO_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `PROXIO_DIAL_TIMEOUT` | `10s` | TCP connect and TLS handshake to the target |
| `PROXIO_RESPONSE_TIMEOUT` | `30s` | How long a target has to **begin** answering. Not a cap on the response |
| `PROXIO_MAX_REDIRECTS` | `10` | `0` passes a `3xx` through untouched |

There is no config file. Five variables do not need one.

**There is no public URL**, and its absence is worth noticing beside the sibling projects,
which all need one badly. proxio builds no links, sets no cookies and signs nothing, so it
never has to know its own address — which also means moving it costs nothing.

## Port

`:80`, inside the container, not configurable. Remap it with `-p`. A port number inside a
container is not a thing an operator should have to think about twice.

## Volume

`/data`, holding `tokens.json` and nothing else.

```sh
docker run -d --name proxio \
  -v proxio-data:/data \
  -p 8080:80 \
  ghcr.io/reeywhaar/proxio:latest
```

**Mount it or proxio will not start:**

```
proxio: /data does not exist: mount a volume there, e.g. `docker run -v proxio-data:/data`, or set PROXIO_DATA_DIR
```

That check is why the Dockerfile deliberately declares **no `VOLUME /data`** and does not
create the directory. With either, Docker makes an anonymous volume, `/data` always exists and
the check can never fire — leaving the tokens in a directory that disappears with the
container, which is the exact failure it exists to prevent.

The writability probe is a real file rather than a look at the mode bits, so a read-only mount
fails at startup rather than at the first `token create` six weeks later.

Backing it up is `cp`. It is one small JSON file with no write-ahead log and no consistency
problem — nothing like the sibling projects' SQLite. Losing it means minting new tokens and
reconfiguring whatever held the old ones.

## Behind caddy-docker-proxy

[`docker-compose.yml`](../docker-compose.yml) is a working deployment: every setting present
and commented out at its default, so there is one file to read rather than a compose file
pointing at an env file that holds the explanations.

```yaml
services:
  proxio:
    image: ghcr.io/reeywhaar/proxio:latest
    restart: unless-stopped
    volumes:
      - data:/data
    networks:
      - caddy
    healthcheck:
      test: ["CMD", "proxio", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
    labels:
      caddy: proxio.example.com
      caddy.reverse_proxy: "{{upstreams 80}}"

volumes:
  data:

networks:
  caddy:
    external: true
```

caddy gets the certificate and terminates TLS; proxio speaks plain HTTP behind it and never
learns its own hostname. Nothing is published to the host — the `networks` list is the reach a
token buys, so keep it short.

`X-Forwarded-For` written by caddy is kept and appended to, so a target sees the real caller
first in the chain, then proxio's own peer.

## Logs

JSON on stdout, one line per proxied request:

```json
{"time":"2026-09-06T00:00:00Z","level":"INFO","msg":"proxied","token":"some_service",
 "method":"GET","url":"https://api.example.com/thing","status":200,"bytes":48122,
 "dur_ms":312,"hidden":false,"client":"10.0.0.7"}
```

`redirects` and `final_url` appear only when a redirect actually moved it.

Three things worth knowing:

- **`token` is the label, never the secret.** That is what labels are for.
- **`client` is logged even when `hidden` is true.** `hide` governs what the target is told,
  not what the operator can see.
- **Of the target URL, only the scheme, host and path are logged.** The query string is left
  out whole and marked `?{redacted}`, and userinfo goes with it — so a chained hop's token,
  the nested URL it wraps, a caller's `?api_key=`, and a `user:password@` are all absent by
  one rule rather than by a list of names nobody maintains.
- **proxio never logs its own request URI**, which is where its own token is. What sits in
  front of it does: see [Read this first](#read-this-first).

`msg` is one of `proxied`, `refused`, `the response stopped part way`, or `the caller hung
up`. Refusals log at `WARN` with no token material at all, not even a prefix of the rejected
string.

## The image

Two stages, ~7 MB.

- **Go stage** cross-compiles from `$BUILDPLATFORM`, `CGO_ENABLED=0`, `-trimpath`, version
  stamped through `-ldflags`. Nothing runs under emulation.
- **`COPY` path by path**, not a `.dockerignore`. An allowlist cannot accidentally admit a
  local `data/` holding real tokens.
- **Alpine runtime with `ca-certificates`**, which is load-bearing here in a way it is not in
  a service that only answers requests: without a certificate store every `https://` target
  fails verification and proxio proxies nothing but plaintext.
- **`HEALTHCHECK` runs `proxio healthcheck`**, so the image needs no `curl` and a wedged
  process fails the check rather than passing it.
- **No `VOLUME`**, for the reason above.

## Shutdown

`SIGTERM` or `SIGINT` stops accepting connections and gives in-flight requests ten seconds,
matching Docker's default stop timeout. A longer grace would only postpone the same `SIGKILL`
for a request that is still streaming.

## CI

`test` → `publish` → `notify`, on every push to `main`.

`test` is `gofmt -l`, `go vet ./...` and `go test ./...`. `publish` builds
`linux/amd64,linux/arm64` and pushes to GHCR with the built-in `GITHUB_TOKEN` — no secret to
provision, which is the whole reason the image lives there.

**The smoke tests are steps on `publish`, not a job of their own**, because a fresh GHCR
package is private and a separate job would have to sign in again to pull the image it just
pushed. `traefik/whoami` is the target: it echoes the request it received as plain text, which
turns every header rule in [proxying.md](proxying.md) into a `grep`.

Twelve assertions, of which two would otherwise ship looking exactly like a successful
publish:

- **A container with no volume must refuse to start.** The one behaviour that cannot be tested
  any other way, because it exists only as an absence in the Dockerfile.
- **`hide=ture` and a bare `hide=` must both be a `400`.** Under a truthy-only check each is
  a `200` with forwarding quietly still on, which is the caller's address going out.

The rest: `version` runs, `/healthz` answers, an empty instance says it has no tokens, a token
minted by `docker exec` works without a restart and stops working when deleted, the token is
never echoed by the target, `X-Forwarded-For` and `Via` appear by default and nothing does
under `hide=1`, and one real `https://` fetch proves the certificate store made it into the
image.

`notify` is a job of its own rather than steps on `publish`, because a failing `test` *skips*
`publish` rather than failing it, and a step in a job that never starts cannot report that it
never started.
