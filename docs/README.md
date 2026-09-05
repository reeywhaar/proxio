# Documentation

How this project is built, so a decision made once does not have to be re-argued.

These are rules and references, not a plan — what gets built in what order is not settled
here. That lives in `private/plans/`.

They belong in the repository for a reason: a document explaining why a header is dropped
belongs beside the code that drops it, in the same history, reviewable in the same diff. That
is also what makes the rule below enforceable — a stale document is something a commit can be
seen not to have fixed.

| document | what it settles |
| --- | --- |
| [proxying.md](proxying.md) | **The contract.** The three parameters, chaining, every header rule, streaming, redirects, errors |
| [tokens.md](tokens.md) | The file, the hashing, how a running server sees `docker exec` |
| [deploy.md](deploy.md) | Image, environment, the `/data` requirement, caddy, logs, CI |
| [stack.md](stack.md) | Every dependency and why. One line long |
| [conventions.md](conventions.md) | Naming, commits, comments, Go rules, time |

proxio is small — about 1,500 lines of Go under `internal/` and as many again in test beside
them — and these documents are short because the program is. Where one runs long it is because the
thing it describes has a version that looks right and is wrong.

## What proxio is, in three sentences

proxio takes a token and an encoded URL and makes that request on the caller's behalf,
streaming the answer back with the method, the headers and the body intact. It exists so that
one machine can reach the internet through another without either of them having to speak a
proxy protocol — an ordinary `GET` to an ordinary URL is the whole interface.

It is not a forward proxy, it does not filter where it will go, and a token is the entire
security boundary. All three are choices, and [deploy.md](deploy.md#read-this-first) says what
the third one costs.

## When these disagree with the code

The code is right and the document is stale. Fix the document in the same commit that made it
stale — a reference nobody trusts is worse than no reference, because it costs a reader the
time to find out.

That is the rule and it will be broken, which is what [meta.txt](meta.txt) is for. It records
the commit each of these was last checked against, so "what has happened since anybody read
this" is `git log <hash>..HEAD` rather than a re-read of everything. Update the line when you
update the document.
