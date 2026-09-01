.DEFAULT_GOAL := test

.PHONY: build test test-race bench lint container-test clean cypher-fetch cypher-generate cypher-generate-check cypher-tck cypher-tck-check

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

cypher-fetch:
	./tools/cypher/fetch.sh

cypher-generate:
	./tools/cypher/generate.sh

cypher-generate-check:
	./tools/cypher/check-generated.sh

cypher-tck: cypher-fetch
	go run ./tools/cypher/tckreport \
		-archive .cache/opencypher/M23/tck-M23.zip \
		-fixtures .cache/opencypher/M23/graphs \
		-manifest tools/cypher/capabilities.json

cypher-tck-check:
	./tools/cypher/check-tck-report.sh

clean:
	go clean -testcache
	rm -f bin/sheets
