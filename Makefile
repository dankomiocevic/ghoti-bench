.PHONY: build test memory-read counters token-buckets

build:
	go build -o bin/ ./cmd/...

test:
	go test -race ./...

# The full first benchmark: 1, 100 and 1000 connections, 30s warm-up,
# 5 minute measurement, three repetitions (about 55 minutes).
memory-read: build
	./bin/ghoti-bench run --scenario memory-read --ghoti-ref $(or $(REF),v0.2.0)

# Both counter benchmarks with the full matrix (about 2 hours).
counters: build
	./bin/ghoti-bench run --scenario counter-hot --ghoti-ref $(or $(REF),v0.2.0)
	./bin/ghoti-bench run --scenario counter-spread --ghoti-ref $(or $(REF),v0.2.0)

# Both token bucket benchmarks with the full matrix (about 2 hours).
token-buckets: build
	./bin/ghoti-bench run --scenario token-bucket-hot --ghoti-ref $(or $(REF),v0.2.0)
	./bin/ghoti-bench run --scenario token-bucket-spread --ghoti-ref $(or $(REF),v0.2.0)
