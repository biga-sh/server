<div align="center">

# biga server

**The API behind [biga](https://github.com/biga-sh/cli).**

Stores broadcast sessions and streams every save to viewers, live.<br>
One Go binary on Postgres. Redis is optional.

[![CI](https://img.shields.io/github/actions/workflow/status/biga-sh/server/ci.yml?branch=main&style=flat-square&labelColor=0b0c0e)](https://github.com/biga-sh/server/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/biga-sh/server?style=flat-square&labelColor=0b0c0e)](LICENSE)

</div>

The [CLI](https://github.com/biga-sh/cli) uploads a snapshot of a folder,
then a checkpoint for every burst of saves. This server stores them
(content-addressed, so a file saved back to an earlier state costs nothing)
and relays each one to the session's viewers over server-sent events. The
[website](https://github.com/biga-sh/website) is the viewer that renders
them.

To run the whole stack on your own machine or network, use
[biga-sh/docker](https://github.com/biga-sh/docker). This repository is the
server on its own.

## Run it

You need Go 1.26 or newer and a Postgres database (13 or newer; Neon works).

```sh
make db                                   # optional: a throwaway Postgres in Docker
export DATABASE_URL='postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable'
go run ./cmd/biga-server                  # migrates, then serves on :8080
```

Or the image:

```sh
docker run -p 8080:8080 -e DATABASE_URL=... ghcr.io/biga-sh/server:1
```

The server applies its migrations on start.

## Who can do what

Reads are public. Holding a session's ID is the permission to watch it, which
is what makes a link shareable without accounts.

Writes are not. Before this server, anyone who knew a session ID could
overwrite its files, reactivate it or end it. Now:

- **Owner tokens.** Creating a session (`PUT /api/sessions/:id` on a new ID)
  returns a random 256-bit token, once. The server stores only its SHA-256.
  Every later write to that session (snapshot, checkpoint, heartbeat, end,
  reactivate, manifest) must send it as `Authorization: Bearer <token>`. The
  CLI keeps it in `~/.biga/sessions.json`, which only your user can read.
- **Access keys (optional).** Set `BIGA_ACCESS_KEYS` and every write also needs
  `X-Biga-Key: <key>`, so only people you gave a key to can broadcast through
  your server. Viewers never need one. Generate keys with
  `biga-server gen-key`; revoking a key stops its broadcasts at the next
  request.
- **Sessions without a token** (rows created before owner tokens existed)
  stay viewable but refuse writes with `403 session_locked`; the CLI responds
  by starting a new session.

Every write is authorized before its body is read, so an unauthorized client
cannot make the server decompress and parse megabytes. More on the threat
model in [SECURITY.md](SECURITY.md).

### What else is checked

| Check | Why |
| --- | --- |
| Session IDs match `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` | No path tricks through the URL. |
| File paths are relative, with no `.`, `..` or empty segments, backslashes or NUL bytes | No traversal, and nothing Postgres refuses to store. |
| A text file's hash must be the SHA-256 of its content | Blobs are served by hash as immutable; a mismatched pair would poison every cache in front of the server. |
| Snapshot trees are checked node by node: depth, file count, duplicate paths, each path matching its place in the tree | Malformed trees are refused before anything is stored. |
| Body limits apply after gzip decompression | A small gzip bomb cannot expand past the limit. |
| Per-session storage quota | One session cannot fill the disk. |
| Fixed-window rate limits per session and per client; repeated bad tokens or keys lock the client out | Brute force and floods. IPv6 clients are counted per /64. |
| `X-Forwarded-For` is believed only from `BIGA_TRUSTED_PROXIES` | Clients cannot pick their own address to dodge limits. |
| Live streams are capped overall and per client; slow readers are dropped | One client cannot hold every connection. |
| Line-diff statistics run under a work budget | A rewritten 2 MB file cannot pin a CPU on every save. |
| Security headers on every response, CORS only on reads, no CORS on writes | The API is JSON only and is never framed. |

## Configuration

Everything is an environment variable. Secrets can also come from a file: set
`NAME_FILE` to its path (how Docker and Kubernetes secrets are mounted).

### Required

| Variable | Description |
| --- | --- |
| `DATABASE_URL` | Postgres connection string. pgx pool settings such as `pool_max_conns` go in its query string. |

### Network

| Variable | Default | Description |
| --- | --- | --- |
| `BIGA_LISTEN` | `:8080` | Comma-separated addresses. `:8080` is every interface, IPv4 and IPv6. Also `127.0.0.1:8080`, `[::1]:8080`, `192.168.1.10:8080`, `tcp4:…`/`tcp6:…` to pin a family, or `unix:/run/biga/biga.sock`. |
| `BIGA_PUBLIC_URL` | unset | The origin viewers open, e.g. `https://biga.example.com`. Returned to the CLI as the share link. |
| `BIGA_TRUSTED_PROXIES` | unset | Peers whose `X-Forwarded-For` is believed: IPs, CIDRs, `loopback`, or `private` (RFC 1918, CGNAT/Tailscale, IPv6 ULA, loopback). Set this when running behind a reverse proxy, or every client looks like the proxy. |
| `BIGA_CORS_ORIGINS` | unset | Browser origins allowed to read the API from another origin, or `*`. Only needed when the viewer and the API are on different origins. |
| `BIGA_TLS_CERT_FILE`, `BIGA_TLS_KEY_FILE` | unset | Terminate TLS in the server itself instead of a reverse proxy. |

### Access

| Variable | Default | Description |
| --- | --- | --- |
| `BIGA_ACCESS_KEYS` | unset | Comma-separated keys; when set, every write needs one. At least 24 characters each. |
| `BIGA_ACCESS_KEYS_FILE` | unset | A file of keys, one per line; `#` starts a comment. |
| `BIGA_CRON_SECRET` | unset | Enables `GET /api/cron/cleanup` for an external scheduler. |

### Retention

| Variable | Default | Description |
| --- | --- | --- |
| `BIGA_RETENTION_DAYS` | `30` | Days an ended session is kept before deletion. |
| `BIGA_ABANDON_AFTER` | `24h` | Silence after which an active session is marked ended. |
| `BIGA_CLEANUP_INTERVAL` | `1h` | How often the built-in sweep runs; `0` turns it off. Only one instance sweeps at a time. |
| `BIGA_PROTECTED_SESSIONS` | unset | Sessions the sweep never deletes. `BIGA_DEMO_SESSION_ID` is added to them. |

### Limits and tuning

| Variable | Default | Description |
| --- | --- | --- |
| `BIGA_MAX_SNAPSHOT_BYTES` | 32 MiB | Largest snapshot body, decompressed. |
| `BIGA_MAX_CHECKPOINT_BYTES` | 8 MiB | Largest checkpoint body, decompressed. |
| `BIGA_MAX_SESSION_BYTES` | 16 KiB | Largest session create or update body. |
| `BIGA_MAX_FILE_BYTES` | 6 MiB | Largest single file's content. |
| `BIGA_MAX_SNAPSHOT_FILES` | 50000 | Most files in one snapshot. |
| `BIGA_MAX_CHECKPOINT_ENTRIES` | 10000 | Most files in one checkpoint. |
| `BIGA_MAX_SESSION_STORAGE_BYTES` | 1 GiB | Stored content per session, after compression. |
| `BIGA_READ_RATE_LIMIT` | 3000 | Viewer requests per client per minute; `0` turns it off. |
| `BIGA_MAX_STREAMS`, `BIGA_MAX_STREAMS_PER_CLIENT` | 10000, 32 | Open live streams. |
| `BIGA_SESSION_STALE_MS` | 45000 | Silence after which viewers see a broadcast as ended. |
| `BIGA_EAGER_MAX_BYTES`, `BIGA_EAGER_MAX_FILES` | 1.5 MiB, 400 | Sessions under both ship every file's content with the first load. |
| `BIGA_BLOB_CACHE_BYTES` | 64 MiB | In-process cache of file contents. |
| `REDIS_URL` | unset | `redis://` or `rediss://` (Redis or Valkey). Shares live events and rate limits across instances. Not needed for one instance. |
| `BIGA_MIGRATE_ON_START` | `true` | Apply migrations before serving. Set `false` and run `biga-server migrate` yourself if you prefer. |
| `BIGA_LOG_LEVEL`, `BIGA_LOG_FORMAT` | `info`, `json` | `debug`…`error`; `json` or `text`. Logs never include tokens, keys or query strings. |

## Commands

```text
biga-server [serve]      Serve the API (the default)
biga-server migrate      Apply database migrations and exit
biga-server cleanup      Run the retention sweep once and exit
biga-server healthcheck  Exit 0 if the local server answers /healthz
biga-server gen-key      Print a new access key
biga-server version      Print the version
```

`GET /healthz` answers while the process is up; `GET /readyz` also checks
Postgres and Redis.

## API

Every JSON response is wrapped: `{"ok": true, "data": …}` or
`{"ok": false, "error": {"code", "message"}}`. The website's
[API reference](https://biga.sh/docs/api) documents each endpoint; in short:

| Method | Path | Auth |
| --- | --- | --- |
| `PUT` | `/api/sessions/:id` | none to create; owner token to reactivate |
| `DELETE` | `/api/sessions/:id` | owner token |
| `POST` | `/api/sessions/:id/snapshot` | owner token |
| `POST` | `/api/sessions/:id/checkpoint` | owner token |
| `POST` | `/api/sessions/:id/heartbeat` | owner token |
| `GET` | `/api/sessions/:id/manifest` | owner token |
| `GET` | `/api/sessions/:id`, `/state`, `/stream`, `/checkpoints`, `/checkpoints/:seq`, `/file`, `/file/history` | none |

With `BIGA_ACCESS_KEYS` set, every row marked "owner token" or "none to
create" also needs `X-Biga-Key`.

## Development

```sh
make test      # unit tests; the API tests skip without a database
make db        # a throwaway Postgres on 127.0.0.1:55432
make test-db   # every test, including the API against that Postgres
make lint      # golangci-lint
make vuln      # govulncheck
make image     # the container image
```

| Path | What lives there |
| --- | --- |
| `cmd/biga-server` | Flags, wiring, listeners and graceful shutdown. |
| `internal/httpapi` | Routes, auth gates, body limits, rate limits, SSE. |
| `internal/service` | Ownership, ingest (one transaction per upload, with the session row locked), reads, retention. |
| `internal/store` | Postgres queries and the embedded migrations. |
| `internal/wire` | The JSON contract and its validation. |
| `internal/auth` | Owner tokens and access keys. |
| `internal/events` | Live-event fan-out, across instances through Redis. |
| `internal/linediff` | Bounded line-diff statistics. |

## License

[MIT](LICENSE)
