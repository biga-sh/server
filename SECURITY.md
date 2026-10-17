# Security

## Reporting a vulnerability

Please report vulnerabilities privately through
[GitHub security advisories](https://github.com/biga-sh/server/security/advisories/new),
not in a public issue. You will get an answer within a few days.

## Threat model

biga shares a folder as a read-only, live link. There are no accounts. The
server protects three things:

1. **A session's content from tampering.** Only the client that created a
   session can change it.
2. **The server from abuse.** Nobody can use it as free storage, exhaust its
   memory, CPU, disk or connections, or knock other users off it.
3. **Viewers from the server's data.** Nothing a broadcaster uploads can run
   in a viewer's browser through the API.

It deliberately does **not** keep sessions secret from people who have the
link. A session ID is a capability to watch: share it and anyone can read
every file in it. Treat links like the files themselves, and keep secrets out
of broadcast folders (the CLI skips `.env`, keys and similar by default).

### Trust boundaries

| Boundary | Defense |
| --- | --- |
| CLI → server writes | Owner token (256-bit, stored as SHA-256, compared in constant time). Optional server-wide access keys. Authorization happens before the body is read. |
| Anyone → server reads | Public by design. Per-client rate limits, stream caps, validated IDs and query parameters, bounded page sizes. |
| Request bodies | Size caps after decompression, strict JSON, schema and path validation, hash verification of text content, a per-session storage quota. |
| Server → database | Parameterized SQL only; batch writes pass arrays. Each upload is one transaction with the session row locked. |
| Server → browsers | JSON only, `nosniff`, `default-src 'none'` CSP, no framing, CORS on reads only and only for configured origins. |
| Proxies → server | `X-Forwarded-For` is believed only from `BIGA_TRUSTED_PROXIES`. |

### Credentials at rest

- The server stores SHA-256 hashes of owner tokens, never the tokens.
- Access keys live in the server's environment or a mounted file.
- The CLI stores owner tokens in `~/.biga/sessions.json` with `0600`
  permissions in a `0700` directory.
- Logs never contain tokens, keys, request bodies or query strings.

### Known limits

- Rate limits are per client address. Many users behind one NAT share a
  budget; raise `BIGA_READ_RATE_LIMIT` or turn it off on a private network.
- Without TLS (a reverse proxy or `BIGA_TLS_CERT_FILE`), tokens and keys
  cross the network in clear text. Use plain HTTP only on networks you trust.
- A session row without an owner token is read-only forever, because nobody
  can prove they own it.
