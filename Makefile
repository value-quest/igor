.PHONY: build test dist clean

VERSION ?= 2.0.0
LDFLAGS := -s -w -X github.com/value-quest/igor/internal/cli.Version=$(VERSION)

build:
	go build -mod=readonly -ldflags "$(LDFLAGS)" -o igor ./cmd/igor

dist:
	mkdir -p dist
	GOOS=darwin GOARCH=arm64 go build -mod=readonly -ldflags "$(LDFLAGS)" -o dist/igor-darwin-arm64 ./cmd/igor
	GOOS=darwin GOARCH=amd64 go build -mod=readonly -ldflags "$(LDFLAGS)" -o dist/igor-darwin-amd64 ./cmd/igor

test:
	go test -mod=readonly -v ./...

clean:
	rm -rf igor dist
