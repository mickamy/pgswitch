# pgswitch

Per-branch Postgres databases behind one DATABASE_URL, following git switch and every worktree.

[![CI](https://github.com/mickamy/pgswitch/actions/workflows/ci.yaml/badge.svg)](https://github.com/mickamy/pgswitch/actions/workflows/ci.yaml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/mickamy/pgswitch)](https://github.com/mickamy/pgswitch/releases/latest)
[![Sponsor](https://img.shields.io/github/sponsors/mickamy?label=Sponsor&logo=GitHub)](https://github.com/sponsors/mickamy)

## Installation

```sh
go install github.com/mickamy/pgswitch@latest
```

With Homebrew:

```sh
brew install --cask mickamy/tap/pgswitch
```

Prebuilt binaries are on the [releases page](https://github.com/mickamy/pgswitch/releases).

## Usage

pgswitch is a proxy in front of your local Postgres. Each connection is traced to the worktree and branch of the process that opened it and routed to that branch's own copy of the database it asked for, cloned on first use from the database of the branch it came from. The default branch keeps the original databases. When a worktree switches branches, its connections are closed, and pools reconnect to the new branch's databases.

### Getting started

Run pgswitch at login (a launchd agent on macOS, a systemd user unit on Linux):

```sh
PGPASSWORD=postgres pgswitch service install -upstream 127.0.0.1:5432 -admin-user postgres
```

Then change only the port your app connects to:

```
DATABASE_URL=postgres://127.0.0.1:6432/myapp_development
```

`pgswitch serve` runs the same proxy in the foreground instead.

### Where the databases live

For each repository, pgswitch uses the first of:

1. `.pgswitch.toml` at the root of the worktree, committed with the project:

   ```toml
   upstream = "127.0.0.1:5433"
   admin_user = "postgres"
   admin_password = "postgres"
   ```

   `upstream` must be a loopback address. Credentials alone may be set too, for the default server.
2. The Postgres container that Compose started from any worktree of the repository and that publishes its port 5432 to the host, found through the Docker API, with the role and password from its `POSTGRES_USER` and `POSTGRES_PASSWORD`.
3. serve's `-upstream`, `-admin-user` and `PGPASSWORD` (or the password `service install` kept).

pgswitch creates branch databases as that admin role. A superuser is simplest. A role without superuser also works if it owns the databases it clones and has `CREATEDB` and `pg_signal_backend`, except that per-role settings for other roles (`ALTER ROLE ... IN DATABASE ... SET`) cannot be copied. The admin role's password must be ASCII: pgswitch does not apply SASLprep, so SCRAM authentication fails for passwords that SASLprep would change.

PostgreSQL 15 or later is required.

### Apps in Docker

A connection from a container reaches the host from the Docker runtime, not from a process in a worktree. Add the relay to `compose.yaml` and point the app at it:

```yaml
services:
  pgswitch:
    image: ghcr.io/mickamy/pgswitch
    command: relay
  web:
    environment:
      DATABASE_URL: postgres://pgswitch:5432/myapp_development
```

The relay forwards to serve at `host.docker.internal:6432` and tells it which container the connection comes from, so it is routed by the worktree Compose ran in. This works where `host.docker.internal` reaches the host's loopback: Docker Desktop, OrbStack and Colima.

### Commands

```sh
pgswitch status     # live connections: worktree, branch, upstream and database
pgswitch log [-f]   # routing, clone, rename and drop events
pgswitch prune      # drop databases whose branch or repository is gone, after asking
pgswitch service install | uninstall
pgswitch help
```

Databases are never dropped implicitly; `prune` asks first (`-yes` for scripts).

## Development

```sh
make test   # go test -race ./...
make e2e    # end-to-end tests; needs PGSWITCH_E2E_ADDR (and PGSWITCH_E2E_PASSWORD) pointing at a Postgres
make lint   # golangci-lint run
make build  # bin/pgswitch
```

## License

[MIT](./LICENSE)
