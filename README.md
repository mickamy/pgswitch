# pgswitch

Per-branch Postgres databases behind one DATABASE_URL, following git switch and every worktree.

[![CI](https://github.com/mickamy/pgswitch/actions/workflows/ci.yaml/badge.svg)](https://github.com/mickamy/pgswitch/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/mickamy/pgswitch)](https://github.com/mickamy/pgswitch/releases/latest)

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

```sh
pgswitch serve -upstream 127.0.0.1:5432
pgswitch help
pgswitch version
```

pgswitch creates branch databases as `-admin-user` (default: your OS user), with the password from `PGPASSWORD`. A superuser is simplest. A role without superuser also works if it owns the databases it clones and has `CREATEDB` and `pg_signal_backend`, except that per-role settings for other roles (`ALTER ROLE ... IN DATABASE ... SET`) cannot be copied. The admin role's password must be ASCII: pgswitch does not apply SASLprep, so SCRAM authentication fails for passwords that SASLprep would change.

PostgreSQL 15 or later is required.

## Development

```sh
make test   # go test -race ./...
make e2e    # end-to-end tests; needs PGSWITCH_E2E_ADDR (and PGSWITCH_E2E_PASSWORD) pointing at a Postgres
make lint   # golangci-lint run
make build  # bin/pgswitch
```

## License

[MIT](./LICENSE)
