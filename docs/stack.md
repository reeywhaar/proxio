# Stack

Every dependency is a thing that can break, change under us, or need explaining to somebody
reading this in a year. This list is one line long.

| what | version | why |
| --- | --- | --- |
| Go | 1.27 | — |
| `net/http` | stdlib | Both halves of proxio: the server and the client it proxies with |
| `log/slog` | stdlib | Structured logging without a dependency |
| `crypto/rand`, `crypto/sha256`, `crypto/subtle` | stdlib | Minting and checking a token — see [tokens.md](tokens.md#hashing) |
| `github.com/spf13/cobra` | v1.10.2 | `serve`, `token`, `healthcheck` — subcommands rather than flags, so `docker exec proxio proxio token create x` reads as what it does |

**One direct dependency.** The image is about 7 MB.

## Not used, deliberately

- **No `httputil.ReverseProxy`.** The obvious thing to reach for, and the wrong shape. It
  exists to put a *fixed* backend behind a fixed front, and its `Rewrite` hook is a place to
  adjust a request whose destination is already known. Here the destination arrives in a query
  parameter and changes every request, and the rules that matter — what an unrecognised
  `hide` does, which errors are proxio's own, whether a `3xx` is followed, what reaches the
  log — are all things it would talk you out of rather than help with.
- **No router.** Two exact paths and a catch-all, which is three lines of `http.ServeMux`.
- **No database.** A list of five rows. [tokens.md](tokens.md#it-is-a-file-rather-than-a-database)
  has the arithmetic: SQLite would be more than half the image.
- **No config file.** Five environment variables. See [deploy.md](deploy.md#environment).
- **No CORS middleware.** The target's policy is relayed rather than replaced, which is what
  makes `fetch()` work against a CORS-permissive target and correctly fail against one that
  is not — see [proxying.md](proxying.md#cors-and-browsers).
- **No metrics endpoint, no rate limiter, no cache.** The access log is the record. A cache is
  a second product.
- **No `golang.org/x/*` at all.** Nothing here needs it, which is unusual enough to be worth
  writing down.

## Not in the program, deliberately

The full list of what proxio will not do, and why, is in the README under *What it
deliberately does not do*. The short version: no forward-proxy mode, no browser support, no
WebSocket relay, no per-token host rules, no destination filtering.
