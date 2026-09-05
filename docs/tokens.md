# Tokens

A token is proxio's whole access control. There is one kind, it is either valid or it is not,
and holding one grants everything proxio can do.

## The command line

```sh
docker exec proxio proxio token create some_service
docker exec proxio proxio token list
docker exec proxio proxio token list --json
docker exec proxio proxio token delete some_service
```

The doubled `proxio proxio` is not a typo: `docker exec` bypasses the image `ENTRYPOINT`, so
the binary has to be named.

`create` prints **the secret alone on stdout** and everything else on stderr, so this captures
the token and nothing else:

```sh
TOKEN=$(docker exec proxio proxio token create some_service)
```

**It is shown once.** What is stored is a hash, so a lost token is deleted and minted again
rather than recovered.

Every subcommand — `serve` included — requires the data directory to exist and be writable,
so a container started without a volume fails on all of them alike.

## Labels

A label is required, unique, and the key: `delete` takes one, and the access log records one.
It may hold letters, digits, `_`, `-` and `.`, up to 64 characters.

**There are no ids**, which is a deliberate break from the sibling projects. An id exists to
name a row in a log without exposing a secret, and a label already does that job better
because a person chose it to mean something. `t_01JQ8…` in an access log answers no question
that `some_service` does not answer faster.

Give each service its own, named after that service. One can then be withdrawn without
disturbing the others, and a line in the log says who was doing what.

## The file

`/data/tokens.json`, mode `0600`:

```json
{
  "version": 1,
  "tokens": [
    {
      "label": "some_service",
      "hash": "4dd1d240ba277eb4d831e96974b0ca6a70d17f7635a1f18ed4fa0a72d558bd5b",
      "created_at": 1757116800
    }
  ]
}
```

`version` is written so that a future format change is a migration rather than a crash on a
field that used to mean something else. Per-token host rules are the change it is being kept
for. A file whose version proxio does not speak stops it starting.

**A missing file is not an error** — that is a fresh volume, and it means no tokens. proxio
warns at startup and answers `401` to everything until one is created; refusing to start would
crash-loop the container before anybody could `exec` into it to make the first one.

### It is a file rather than a database

The sibling projects use SQLite. Here that would cost about twelve megabytes of binary and a
migration framework to manage one table with five rows in it. The image is 7 MB as it stands.

The one thing a file has to do that a table would do for free is be visible to a second
process, and that is the next section.

## How a running server sees `docker exec`

`docker exec proxio proxio token create x` is **not the running server**. It writes the file;
the server has the old one in memory.

**proxio stats the file before each authentication and re-reads it when the timestamp or size
has changed.** That costs about a microsecond against a proxied request that costs
milliseconds, and there is no state in which the server is stale. A token works, or stops
working, on the next request — never after a restart.

The alternatives were a file watcher, which is a dependency and relies on inotify behaving on
an overlay mount, and a signal, which works and is forgotten exactly once, by the person who
then spends an hour on why their new token is rejected.

Writes go through a temporary file in the same directory, fsync, and a rename, so the server
can never read a half-written file.

**Two `token create` runs racing each other can lose one.** Each re-reads before appending, so
the ordinary case of creating a token months after the last one is safe; what is not covered
is two shells writing in the same instant. Token management is a person typing into a
terminal, and the file lock that would close that window costs more than the window is worth.

## Hashing

The secret is `px_` followed by 32 random bytes in base64url — 43 characters, 256 bits.

Stored as **SHA-256, hex**. Not bcrypt, and the difference from how the sibling projects hash
a password is the whole point: bcrypt exists to make a low-entropy secret that a *person*
chose expensive to guess, and a 256-bit random string is not guessable at any cost. A slow
hash here would buy nothing and be paid for by every proxied request.

Verification hashes the presented string and compares it against every stored digest with
`subtle.ConstantTimeCompare`, without stopping at a match — an early return would make the
reply time depend on how far down the file a token sits.

## When the file is damaged

Two different answers, because they are two different situations:

- **At startup**, a corrupt or unparseable file stops `serve`. It is a configuration failure,
  and a server that came up anyway would refuse every request for a reason nobody could see.
- **While running**, the tokens already in memory keep working and the failure is logged at
  `ERROR` naming the path. A botched hand-edit does not become an outage. The complaint is
  made once per change to the file rather than once per request.

The supported way to change tokens is the command line, which writes atomically and cannot
produce either state.
