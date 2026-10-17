<picture>
  <source media="(prefers-color-scheme: dark)" srcset="https://website-vw4l.vercel.app/brand/biga-logo-light.svg">
  <img src="https://website-vw4l.vercel.app/brand/biga-logo.svg" width="180" alt="biga">
</picture>

# The API behind biga

The biga server powers shared code sessions. It handles API requests, stores snapshots, and streams updates to connected viewers.

[Website](https://website-vw4l.vercel.app/) · [Documentation](https://website-vw4l.vercel.app//docs) · [Issues](https://github.com/biga-sh/server/issues)

## Responsibilities

- Manage sharing sessions and their metadata.
- Store and retrieve code snapshots.
- Deliver session updates to connected viewers.
- Enforce configured access controls, limits, and retention policies.

For the system architecture, API contracts, and end-to-end behavior, refer to the [biga documentation](https://website-vw4l.vercel.app/docs/architecture).

## Requirements

Use the versions and services supported by the current project configuration. A typical deployment requires Go and PostgreSQL. Multi-instance deployments may also use Redis-compatible infrastructure where configured.

## Configuration

Configure the server through the environment variables supported by the current version. These may include:

- `DATABASE_URL` — database connection.
- `BIGA_LISTEN` — server bind address.
- `BIGA_PUBLIC_URL` — externally reachable server URL.
- `BIGA_ACCESS_KEYS` — configured access keys.
- `BIGA_RETENTION_DAYS` — snapshot retention policy.
- `REDIS_URL` — Redis-compatible service, when enabled.

Check the existing configuration and deployment documentation for exact formats, defaults, and required variables.

**Security:** Do not commit credentials or production secrets. Configure access controls, retention, and network exposure deliberately before deploying a public instance.

## Development

Clone the repository and follow the project’s documented setup instructions. Configure PostgreSQL and the required environment variables before running the server.

Run the available tests and checks before submitting changes.

## Contributing

Bug reports, focused improvements, and pull requests are welcome.

- [Documentation](https://website-vw4l.vercel.app/docs)
- [Browse the source](https://github.com/biga-sh/server)
- [Report a bug](https://github.com/biga-sh/server/issues)
- [Report a vulnerability](https://github.com/biga-sh/server/security/advisories/new)

---

<p align="center">The infrastructure behind <strong>biga</strong>.</p>
