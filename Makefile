.PHONY: build test memory-read

build:
	go build -o bin/ ./cmd/...

test:
	go test -race ./...

# The full first benchmark: 1, 100 and 1000 connections, 30s warm-up,
# 5 minute measurement, three repetitions (about 55 minutes).
memory-read: build
	./bin/ghoti-bench run --scenario memory-read --ghoti-ref $(or $(REF),v0.2.0)
