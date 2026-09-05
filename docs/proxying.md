# Proxying

What crosses, what does not, and why. This is the contract; everything else in proxio exists
to keep it.

## The endpoint

```
<any method> /proxy?url={percent-encoded absolute URL}&token={token}[&hide=1]
```

Exact path. `/proxy/`, `/proxy/anything` and every other path are `404`, so there is no second
way in.

Three parameters, and everything else in the request belongs to the caller or to the target:

| parameter | required | meaning |
| --- | --- | --- |
| `url` | yes | The absolute `http` or `https` URL to fetch, percent-encoded |
| `token` | yes | The credential. Missing or unknown is a `401` |
| `hide` | no | `1` tells the target nothing about who asked |

**Any of the three given more than once is a `400`.** Which copy wins is a request-smuggling
question and every answer to it is a bug somebody eventually finds — and for `token`, "first
wins" and "last wins" are two different access decisions from the same URL.

The target's own query string survives because `url=` carries a single opaque value. A target
with `?a=1&b=2` is written `%3Fa%3D1%26b%3D2`, so it cannot be confused for proxio's
parameters nor they for it.

```sh
curl "https://proxio.example.com/proxy?token=$TOKEN&url=$(printf %s 'https://api.example.com/things?page=2' | jq -sRr @uri)"
```

### Why parameters and not headers

Because a proxy has to be able to stand in front of a proxy.

With the credential in a header, chaining is not merely awkward, it is impossible: the second
proxio can only see a token if the first one forwards it, and forwarding a credential to
whatever host the caller named is the one thing a proxy must never do. In the URL, each hop
carries its own and neither knows the other's. See [Chaining](#chaining).

It also means anything that can fetch a URL can use proxio — a shell, a `<video>` tag, a
config file with no header field — at the cost documented under
[Where the token ends up](#where-the-token-ends-up).

## Authentication

`token` and nothing else. Missing, empty, or unrecognised is a `401` — **the same status and
the same body for all three**, because the difference tells whoever is guessing which half
they got right.

The token is checked **before the target URL is even parsed**, so a stranger learns nothing
about what proxio would or would not have been willing to fetch.

`/healthz` is the one unauthenticated path.

## `hide`

Off by default. On, proxio tells the target nothing about who asked.

| `hide` | the target sees |
| --- | --- |
| absent, `0`, `false`, `no`, `off` | `X-Forwarded-For` with proxio's peer appended to any inbound chain, `X-Real-Ip`, and `Via: 1.1 proxio` |
| `1`, `true`, `yes`, `on` | none of those, **and** any inbound `X-Forwarded-For`, `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Real-Ip`, `Forwarded` and `Via` **stripped** rather than passed along |
| anything else, **including empty** | **`400`** |

Case-insensitive.

**The third row is the point, and the empty case is the one a parameter has that a header did
not.** `?hide` with no value is the natural way to write a flag; read as off, it would send
the address of somebody who believed they had hidden it. So `?hide` is refused with a message
saying to write `hide=1`, and so is `hide=ture`. A parameter whose only job is concealment has
to fail loudly when it is not understood.

**Stripped, not merely not-added**, is the other half. A caller sitting behind something that
already wrote an `X-Forwarded-For` — another proxio, very possibly — would otherwise have its
address forwarded by the header it arrived in.

### What `hide` does not cover

`Referer`, `Origin`, `User-Agent`, `Cookie` and `Authorization` are inherited in **both**
modes.

The line is *headers proxio would add or relay about the connection*, not *everything
identifying*. Those five are set by the caller, who can unset them; the forwarding headers are
added by proxio, who otherwise cannot be told not to. If you need `Referer` gone, do not send
one — proxio will not remove it for you, and believing otherwise is a leak you cannot see.

`hide` also says nothing about proxio's **own logs**, which record the caller's address either
way. It governs what the target is told, not what the operator can see.

## Chaining

A proxio's target can be another proxio's `/proxy` URL. That is the reason the credential
lives in the URL rather than in a header, and it needs no feature of its own — it falls out of
the interface.

```
caller ──▶ proxio A ──▶ proxio B ──▶ target
        token=A       token=B
```

The outer URL carries A's token and, percent-encoded inside it, the whole of B's URL with B's
token:

```sh
inner=$(printf %s "https://b.example.com/proxy?token=$B_TOKEN&url=$(printf %s "$TARGET" | jq -sRr @uri)" | jq -sRr @uri)
curl "https://a.example.com/proxy?token=$A_TOKEN&url=$inner"
```

**Each hop decides for itself.** `hide=1` on the outer URL hides the caller from B; `hide=1`
on the inner hides A from the target. Neither implies the other, and A never learns B's token.

`Via` gains an entry per hop and `X-Forwarded-For` grows by one, so the chain reads as a chain
rather than as its last link — unless a hop was asked to hide, which resets what the next one
can see.

**`X-Proxio-Error` says a hop failed, not which.** The body says what went wrong; the hop that
generated it is the first one whose own request failed.

### Where the token ends up

A credential in a URL is in every place a URL goes. This is the cost of the interface and it
is not one proxio can pay on your behalf.

**proxio's own logs are clean, on both counts.** It never logs its own request URI — which is
where its own token is — and of a *target* URL it logs only the scheme, host and path:

```json
{"msg":"proxied","token":"some_service","method":"GET",
 "url":"https://b.example.com/proxy?{redacted}","status":200}
```

The query string is left out **whole** rather than filtered, which is what makes it safe
without proxio having to know whose secret is whose. A chained target carries the next hop's
token *and* the entire nested URL, and both live in the query — so one rule removes the
credential, the nesting, and any `?api_key=` a caller happened to put on an ordinary target.
Building the line from the URL's parts also drops userinfo, so
`https://user:password@example.com/` does not put a password in the log.

The `?{redacted}` marker is there so a reader can tell a URL that had a query from one that
did not, rather than going looking for a request that matches nothing they sent.

**What is in front of proxio is not clean.** caddy, or any reverse proxy, logs the full
request URI including the query string, and that is where the token actually is. Filter it out
or turn that access log off — Caddy's `filter` log encoder can redact a query parameter from
`request>uri`. The same goes for browser history, shell history, and anywhere a URL gets
pasted.

If that trade is not acceptable for a particular caller, that caller should not be given a
token.

## Request headers

Copied verbatim, except:

- **Hop-by-hop, per RFC 9110 §7.6.1**: `Connection`, `Keep-Alive`, `Proxy-Authenticate`,
  `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`.
- **Everything named in the inbound `Connection` header.** The half that gets forgotten: a
  proxy that drops the fixed list but forwards a header the sender explicitly asked to have
  consumed at this hop is passing on something nobody meant to send onward.
- **`Host`**, which is not copied because it is not there to copy: Go moves the inbound
  authority to `Request.Host` and deletes it from the map. The outbound `Host` is the target's.
- **`Content-Length`**, which travels as a length rather than a header. A chunked upload
  arrives with an unknown length and goes out chunked.

Nothing else is special-cased. proxio reads no request header, so there is no header of
proxio's to strip.

`Accept-Encoding` crosses untouched, and proxio's transport runs with compression disabled, so
proxio neither asks for an encoding nobody wanted nor transparently decompresses one that was.
A caller that asked for gzip gets gzip bytes and the `Content-Encoding` that describes them.

**A request with no body sends no body.** Handed a reader of unknown length, Go would send
`Transfer-Encoding: chunked`, and a chunked `GET` is a request some servers answer with a
`400`.

## Response headers

Everything the target said, except the same hop-by-hop set. `Set-Cookie`, `Content-Encoding`,
`Content-Length` and `Content-Type` all cross unchanged, as does the status code.

**Trailers are not relayed.** A known gap, not an oversight.

## Streaming

A byte the target produces reaches the caller without waiting for the byte after it. Four
things have to be true together, and each has a way of quietly not being:

1. **The request body is streamed**, never read into memory. An upload of any size costs
   proxio no memory and no disk.
2. **The response is copied through a writer that flushes every chunk.** Without the flush,
   Go's response buffer holds up to four kilobytes — and holds the status line with it — which
   turns a server-sent-event stream into silence that looks exactly like a slow origin.
3. **The server has no read or write timeout.** Either one is a ceiling on how long a proxied
   request or response may last, and the failure — a download cut at exactly N seconds — gets
   blamed on the network for a week. What remains is a 10 s header timeout and a 120 s idle
   timeout, which bound the things that genuinely should be bounded.
4. **A caller that hangs up cancels the fetch**, instead of leaving proxio pulling a file
   nobody is reading. In a chain this propagates the whole way down.

`PROXIO_RESPONSE_TIMEOUT` bounds the wait for the *first* byte, which is the part that can be
bounded without breaking a long response. There is no ceiling on the body, by design, and no
request or response size limit — a proxy that caps one is a proxy that fails on the one upload
somebody cared about.

## Redirects

Followed inside proxio, up to `PROXIO_MAX_REDIRECTS` hops (default 10). The caller sees the
final response and never has to re-encode a `Location`. Exceeding the limit is a `502` naming
the number.

`PROXIO_MAX_REDIRECTS=0` turns following off: the `3xx` passes through with its `Location`
untouched, and following it is the caller's business.

Two behaviours are inherited from Go's HTTP client rather than written here, and both are
surprises in something claiming to be transparent:

- **A body-carrying request that meets a `307` or `308` gets the redirect, not the final
  response.** Replaying the body would mean buffering it, and buffering an upload of unknown
  size is the one thing streaming rules out. Documented rather than patched, because the
  alternative silently caps upload size.
- **A `POST` that meets a `301`, `302` or `303` is followed as a `GET` with no body**, which
  is what browsers do. The caller sees the answer to a request it did not quite make.

Cross-host hops drop `Authorization` and `Cookie`.

## Errors

proxio's own failures carry **`X-Proxio-Error`** and a JSON body:

```
HTTP/1.1 502 Bad Gateway
X-Proxio-Error: upstream
Content-Type: application/json

{"error":"dial tcp: connection refused"}
```

| status | `X-Proxio-Error` | when |
| --- | --- | --- |
| `400` | `request` | any parameter repeated; `url` missing, empty, relative, wrong scheme or hostless; `hide` unrecognised or empty |
| `401` | `auth` | `token` missing or unknown |
| `404` | `request` | any path but `/proxy` and `/healthz` |
| `502` | `upstream` | the target could not be reached, or sent too many redirects |
| `504` | `upstream` | the target did not connect or begin answering in time |

The header is the point of the arrangement. On a successful proxy the body belongs to the
target and so does the status, so a caller could not otherwise tell *the proxy failed* from
*the target returned 502*. **proxio never sets this on a relayed response**, so its presence
means the bytes below it are proxio's — or, in a chain, some hop's.

**A response that fails part way through has already sent its status**, and there is nothing
left to say with it. The copy stops, the connection closes without a terminating chunk so the
caller sees a truncated body rather than a complete one, and the reason is in the log as
`the response stopped part way`.

## `/healthz`

```
GET /healthz  →  200 {"ok":true,"version":"a1b2c3d"}
```

Unauthenticated, so a container healthcheck and a load balancer can both reach it. It reports
that the process is answering, which is the only thing proxio can honestly promise: it has no
database to check and no upstream of its own.

## CORS, and browsers

**proxio adds no CORS headers and removes none.** The target's policy is what the browser
sees.

That sentence carries more than it looks, because a browser checks the response it received
for `Access-Control-Allow-Origin` without caring which server sent it:

- `Origin` is forwarded to the target, like every other caller-set header.
- The target answers with whatever CORS headers it would have given a direct request.
- proxio relays them, like every other response header.
- `OPTIONS` is relayed too, so a preflight is answered *by the target* and its
  `Access-Control-Allow-Methods` and friends come back intact.

So **`fetch()` from a page works through proxio exactly where it would have worked against the
target directly.** A target sending `Access-Control-Allow-Origin: *`, or echoing `Origin`, is
readable. A target sending nothing is blocked by the browser, and proxio will not paper over
that.

Together with a token in the query string rather than a header, that makes proxio usable from
a page for most of what it is for: `<img>`, `<video>`, `<audio>`, `<script>` and a plain link
never needed CORS at all, and `fetch()` works against any CORS-permissive target.

Relaying the target's policy grants nothing that a token does not already grant. Reading the
response still needs a valid token in the URL, and a page holding one could read the same
bytes through a server of its own.

### What proxio will not do is manufacture a policy

Synthesising `Access-Control-Allow-Origin: *` would make *every* target readable from *every*
page. Some proxies exist to do exactly that; it is a different product, and it means deciding
on a target's behalf something the target deliberately did not say.

It would also mean answering preflights here, which collides with relaying `OPTIONS` — a
method like any other. If that is wanted, it belongs on a second endpoint with its own rules
rather than as a flag on this one.
