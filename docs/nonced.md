# Nonced tokens

A token can be sent whole, or kept back and proved with a hash. Same token either way — there
is nothing to choose at mint time.

```
?token=px_XCKKOs_wu0DuJ4EUbYDOhwV7hOGbGyrVfeT_BWsOhFE       ← the credential
?token=pxc_1789343452.b7e1cda9.e95a47206414e7d1c60a44…      ← useless in 5 minutes
```

**Here it matters more than it does anywhere that uses a header.** proxio's credential travels
in a URL, because [that is what lets one proxio stand in front of another](proxying.md#chaining)
— and a URL is the one part of a request that everything writes down. caddy redacts
`Authorization`; it logs `request.uri` in full. So the raw form lands in the access log of every
proxy in front of proxio, in browser history, and in every place a URL gets pasted, and it works
forever once it is there.

**That is the whole goal: stop the raw token travelling, so it has nowhere to land.** Not to
authenticate the request, not to prove possession of a key, not to make a replay impossible —
those are different problems with heavier answers.

## Contents

- [The format](#the-format)
- [Making one](#making-one)
- [Chaining](#chaining)
- [Why it needs no second kind of token](#why-it-needs-no-second-kind-of-token)
- [Why plain SHA-256](#why-plain-sha-256)
- [What it does not do](#what-it-does-not-do)
- [When one is refused](#when-one-is-refused)

## The format

```
pxc_1789343452.b7e1cda9.e95a47206414e7d1c60a4424dbeb230e85eebc92ba0b6530bac5d43bf881fb58
└┬─┘└────┬───┘ └───┬──┘ └───────────────────────────────┬──────────────────────────────┘
 │       │         │                                    │
 │       │         │                                    sha256("<nonce>.<id>.<key>")
 │       │         token id — the first 8 of the key
 │       nonce — unix seconds, ±5 minutes
 prefix
```

The **key** is `sha256(secret)`, which is what `tokens.json` already stores for every token. The
caller has the secret and derives the same value; proxio never needs the secret back.

Fields are separated by a dot, which is unreserved in a URL — a colon is a delimiter — so the
value goes into a query parameter without escaping. No field can contain one: they are digits
and hex.

The id is the same one `proxio token list` prints, so a refused request in a log can be traced
to a token without anybody holding the secret.

## Making one

The secret is the only thing a caller needs. `sha256sum` is in every base image including
busybox, which `openssl` is not.

```sh
key=$(printf %s "$SECRET" | sha256sum | cut -d' ' -f1)
id=$(printf %s "$key" | cut -c1-8)
ts=$(date +%s)
tok="pxc_$ts.$id.$(printf '%s.%s.%s' "$ts" "$id" "$key" | sha256sum | cut -d' ' -f1)"

curl "https://proxio.example.com/proxy?token=$tok&url=$(printf %s "$TARGET" | jq -sRr @uri)"
```

```ts
const sha256 = async (s: string): Promise<string> =>
  [...new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s)))]
    .map((b) => b.toString(16).padStart(2, "0"))
    .join("")

export async function proxioToken(secret: string): Promise<string> {
  const key = await sha256(secret)
  const id = key.slice(0, 8)
  const ts = Math.floor(Date.now() / 1000)
  return `pxc_${ts}.${id}.${await sha256(`${ts}.${id}.${key}`)}`
}
```

Web Crypto, so it runs unchanged in Node, Deno, Bun, a worker and a browser.

There is **no `proxio token sign`**. Signing belongs to the caller, who is the only one that
should have the secret — and a command on the server would need none, which is the property
described [below](#why-it-needs-no-second-kind-of-token).

## Chaining

Every hop is independent: each carries its own token in its own URL, and each can be raw or
nonced without the others knowing or caring.

Nonce the inner one too. The inner hop's credential sits inside the outer hop's `url=`
parameter, which is precisely the string a reverse proxy in front of the outer proxio writes
into its log.

```sh
inner="https://b.example.com/proxy?token=$B_TOK&url=$(printf %s "$TARGET" | jq -sRr @uri)"
curl "https://a.example.com/proxy?token=$A_TOK&url=$(printf %s "$inner" | jq -sRr @uri)"
```

Both values are built at request time, so the five-minute window covers the whole chain with
room to spare — a hop forwards immediately, and the nonce is checked when the request arrives
rather than when the URL was written. A chained URL is not something to prepare in advance, and
with nonced tokens it stops being something worth storing at all.

## Why it needs no second kind of token

The obvious first attempt keys the hash on the **secret**, which means proxio has to keep the
secret to check against — a second kind of token, stored in the clear, with its own prefix, its
own mint flag and a `tokens.json` version bump.

Keying on the hash the file already holds removes all of it. Nothing is stored that was not
stored before, every existing token works, and there is nothing to decide when minting one.

What it does mean: **`tokens.json` is enough to use a token.** Whoever can read it can compute
valid nonced values, though they still cannot recover a secret or use it anywhere else. Before,
the file was a list of verifiers and useless on its own. That is the one thing this costs, and
it is the reason the file is `0600` on a volume that is worth the same care as the tokens
themselves.

## Why plain SHA-256

HMAC is the usual answer and needs `openssl(1)`, which is in none of alpine, debian-slim or
ubuntu. The objection to a plain hash is length extension — given `H(k ‖ m)` you can compute
`H(k ‖ m ‖ pad ‖ m')` — and it does not apply here because **the key goes last**: a forged
digest would be for `nonce.id.key‖pad‖extra`, a shape proxio never builds.

Two things are therefore load-bearing rather than tidy: the nonce is parsed **strictly** as
digits, which is what keeps the base unambiguous; and the comparison is **constant time**.

If the base ever grows a field, revisit this — the argument is about *this* base.

## What it does not do

- **It does not authenticate the request.** Only the nonce and the id are covered, so nothing
  binds the value to the URL it arrived on. Anyone who captures one inside the window can point
  it at a different target.
- **It does not prevent replay inside the window.** A captured value works for up to five
  minutes, as with Stripe and Slack. A nonce cache would be state that dies on restart, and the
  problem being solved is the log that is still on disk next week.
- **It does not protect the secret at the caller.** Only the wire value.

Between them those say what the five minutes are for: they close the gap between *a log file is
a credential* and *a log file is a record of requests*.

## When one is refused

The reply is a plain `401` that says nothing — the same status and the same body as a missing
or unknown token, because the difference tells whoever is guessing which half they got right.

**The log says which**, for the value's own shape and timestamp only:

```json
{"level":"WARN","msg":"a nonced token was refused",
 "reason":"the nonce is 1h0m1s from now, outside the 5m0s window; check the clock on the caller",
 "client":"192.168.65.1"}
```

Clock skew is why this exists. A caller five minutes out fails every request, and a reply saying
`missing or names no token` sends somebody looking at their token for an afternoon.

It never reads the token list, so it cannot say whether an id exists and is no oracle for one. A
value it finds nothing wrong with — well-formed, in window, wrong digest — produces no line of
its own beyond the ordinary `refused`.
