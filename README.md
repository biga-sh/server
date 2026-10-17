<p align="center">
  <a href="https://website-vw4l.vercel.app/">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="https://website-vw4l.vercel.app/brand/biga-logo-light.svg">
      <img src="https://website-vw4l.vercel.app/brand/biga-logo.svg" width="180" alt="biga">
    </picture>
  </a>
</p>

<h1 align="center">The API behind biga</h1>

<p align="center">
  A Go service that stores shared sessions and streams every change to the people watching.
</p>

<p align="center">
  <a href="https://github.com/biga-sh/server/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/biga-sh/server/ci.yml?branch=main&style=flat-square&labelColor=0c0d10&label=ci" alt="CI status"></a>
  <a href="https://github.com/biga-sh/server/pkgs/container/server"><img src="https://img.shields.io/github/v/tag/biga-sh/server?style=flat-square&labelColor=0c0d10&color=63e197&label=image" alt="Latest image"></a>
  <a href="go.mod"><img src="https://img.shields.io/github/go-mod/go-version/biga-sh/server?style=flat-square&labelColor=0c0d10&color=63e197" alt="Go version"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-63e197?style=flat-square&labelColor=0c0d10" alt="MIT license"></a>
</p>

<p align="center">
  <a href="https://website-vw4l.vercel.app/">Website</a> ·
  <a href="https://website-vw4l.vercel.app/docs">Documentation</a> ·
  <a href="https://website-vw4l.vercel.app/docs/api">API reference</a> ·
  <a href="https://github.com/biga-sh/server/issues">Issues</a>
</p>

---

The [CLI](https://github.com/biga-sh/cli) uploads a snapshot of a folder and then small checkpoints as files change. The server keeps them in PostgreSQL and pushes each change to viewers over server-sent events. Only the client that created a session can write to it; a link lets people watch, nothing more.

## Get started

### Run the image

```bash
docker run -p 8080:8080 \
  -e DATABASE_URL='postgres://biga:…@db:5432/biga' \
  -e BIGA_PUBLIC_URL=https://biga.example.com \
  -e BIGA_TRUSTED_PROXIES=private \
  ghcr.io/biga-sh/server:1
```

### Build from source

You need Go 1.26 or newer and a PostgreSQL database.

```bash
make build                                        # ./biga-server
DATABASE_URL='postgres://…' ./biga-server
```

The server applies its database migrations on start, answers `GET /healthz` (process up) and `GET /readyz` (PostgreSQL and Redis reachable), and shuts down gracefully on `SIGTERM`. Route `/api/sessions/*` to it and everything else to the [website](https://github.com/biga-sh/website). To run the whole stack at once, use [biga-sh/docker](https://github.com/biga-sh/docker).

## Configuration

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | PostgreSQL connection string. Required. |
| `BIGA_LISTEN` | Addresses to serve on. Default `:8080`. |
| `BIGA_PUBLIC_URL` | Origin viewers open, used to build share links |
| `BIGA_ACCESS_KEYS` | When set, only holders of one of these keys can broadcast |
| `BIGA_TRUSTED_PROXIES` | Peers whose `X-Forwarded-For` is believed |
| `BIGA_CORS_ORIGINS` | Origins allowed to read the API from a browser |
| `BIGA_RETENTION_DAYS` | Days an ended session is kept. Default `30`. |
| `REDIS_URL` | Shares live events and rate limits across instances. Unset runs single-instance. |

Secrets can also be read from a file by appending `_FILE` to the name, which is how Docker and Kubernetes secrets are mounted. Every setting, including limits and TLS, is listed in [configuration](https://website-vw4l.vercel.app/docs/configuration).

## Development

```bash
make test       # unit tests with the race detector; database tests skip
make db         # throwaway PostgreSQL on 127.0.0.1:55432
make test-db    # every test, including the API against PostgreSQL
make lint       # golangci-lint
make vuln       # govulncheck
```

## Documentation

- [Architecture](https://website-vw4l.vercel.app/docs/architecture)
- [API reference](https://website-vw4l.vercel.app/docs/api)
- [Configuration](https://website-vw4l.vercel.app/docs/configuration)
- [Self-hosting](https://website-vw4l.vercel.app/docs/self-hosting)
- [Threat model](SECURITY.md#threat-model)

## Repositories

| Repository | What it is |
| --- | --- |
| [cli](https://github.com/biga-sh/cli) | The `biga` command. Watches a folder and streams every save. |
| [server](https://github.com/biga-sh/server) | The Go API. Stores sessions and streams updates to viewers. |
| [website](https://github.com/biga-sh/website) | The site, the live viewer and the documentation. |
| [docker](https://github.com/biga-sh/docker) | Self-hosting: the whole stack with Docker Compose. |

## Contributing

Bug reports, focused fixes and pull requests are welcome. Read the [contributing guide](https://github.com/biga-sh/.github/blob/main/CONTRIBUTING.md) before you start, and report vulnerabilities privately as described in the [security policy](https://github.com/biga-sh/server/security/policy), not in a public issue.

## License

biga is [MIT licensed](LICENSE).

---

<p align="center">
  <sub><a href="https://website-vw4l.vercel.app/">biga</a> · Made for people who teach, pair and build in public.</sub>
</p>
