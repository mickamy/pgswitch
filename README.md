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

## Development

```sh
make test   # go test -race ./...
make lint   # golangci-lint run
make build  # bin/pgswitch
```

## License

[MIT](./LICENSE)
