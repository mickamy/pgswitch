.PHONY: build install test e2e lint clean

build:
	go build -trimpath -ldflags "-s -w" -o bin/pgswitch .

install:
	go install .

test:
	go test -race ./...

# Needs a Postgres: PGSWITCH_E2E_ADDR=127.0.0.1:5432 PGSWITCH_E2E_PASSWORD=... make e2e
e2e:
	go test -race -tags e2e ./e2e/...

lint:
	golangci-lint run

clean:
	rm -rf bin
