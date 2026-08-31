.DEFAULT_GOAL := test

.PHONY: build test test-race bench lint container-test clean

build:
	go build -trimpath -ldflags "-s -w" -o bin/sheets ./cmd/sheets

test:
	go test ./...

test-race:
	go test -race ./...

bench:
	go test -run '^$$' -bench . -benchmem ./...

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"
	git diff --check

container-test:
	docker build --target test .

clean:
	go clean -testcache
	rm -f bin/sheets

