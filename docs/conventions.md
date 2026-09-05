# Conventions

## Naming things

| word | meaning |
| --- | --- |
| **target** | The URL proxio was asked to fetch. Never "upstream" in prose, never "backend" |
| **caller** | Whoever made the request to proxio. Never "client", which is also `http.Client` |
| **token** | The secret. `Token` in Go, one row in `tokens.json` |
| **label** | The name a token is known by, and the key it is deleted by |
| **relay** | What proxio does to a request. The handler is `relay`, not `handle` or `serve` |
| **hide** | The `hide` parameter's mode. A bool called `hide`, never `anonymous` or `private` |
| **hop** | One proxio in a chain. A chained target is another proxio's `/proxy` URL |

"Proxy" is the program and the verb, and is avoided as a noun for anything inside it — there
is no `proxy` variable, because everything here is one.

## Commit messages

**One line. No body, no trailers, ever.**

A full declarative sentence saying what the change accomplishes. Capitalized, no trailing
period, no prefix, no conventional-commit tag, no ticket number, and no `Co-Authored-By`.

```
Refuse a hide value proxio does not recognise, so a typo cannot silently leak an address
Leave the target's query string out of the log, so a chained hop's token is never written down
Strip an inbound X-Forwarded-For under hide rather than merely not adding one
Send no body on a request that has none, so a GET does not go out chunked
```

Two clauses joined by "so" or "and" are common and welcome — the second says why the first was
worth doing. What the message must not be is a label: not `fix: headers`, not `update proxy`,
not `wip`.

The single line is not a length limit fighting the explanation; it is where the explanation
goes. If a change needs three paragraphs of justification, those paragraphs belong in a
comment beside the code they justify. A change that genuinely cannot be said in one sentence
is usually two changes.

## Comments

Comments explain **why**, never what. A comment restating the line under it is noise; a
comment recording the reason a line is written the way it is prevents somebody "simplifying"
it back into a bug.

A comment is as long as the surprise it explains and no longer. The ones that earn a paragraph
are the ones where the obvious version is wrong, and the paragraph is what stops it being
written back — `flushingCopy` and `hidden` are both that.

When a decision has a real alternative, say what the alternative was and what it cost. That is
the sentence that is impossible to reconstruct later.

Say it once. The same reason repeated in a function, its test and its caller is three copies to
keep true, and the two that fall behind are the ones somebody will read.

Package doc comments are expected and are the right place for the argument a package exists to
make.

## Go

- `gofmt` clean; CI fails on anything it would rewrite.
- Tests beside sources as `*_test.go`. No `tests/` directory.
- Errors wrap with `%w` and name what was being done.
- One error vocabulary in `internal/tokens`: `ErrNotFound`, `ErrConflict`, `ErrInvalid`.
- `context.Context` on anything that can block, and the caller's context on anything done on
  the caller's behalf — which is what makes a hang-up cancel the fetch.
- **A test that would hang has to be made to fail instead.** `TestTheResponseIsStreamed` bounds
  its request with a context for exactly this reason: without the flush, the status line never
  arrives either, so the naive version of that test blocks until the package timeout and says
  nothing about why.
- A function that returns "this succeeded, and also something happened" returns a bool, not a
  sentinel error. `tokens.Verify` returns `(label, ok, error)` where the error means *the file
  could not be re-read*, not *the token was wrong* — folding those together would turn a
  damaged file into an outage.

## Time

- `main.go` pins `time.Local = time.UTC`. Everything logged is UTC.
- Stored as **Unix seconds in an integer field**. Not text, not milliseconds.
- proxio has no notion of local time anywhere and should not acquire one.
