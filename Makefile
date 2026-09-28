.PHONY: all clean test install dist semconv

all: s3rp

s3rp: go.* *.go
	go build -o $@ ./cmd/s3rp/

clean:
	rm -rf s3rp dist/

test:
	go test -race ./...

install:
	go install github.com/fujiwara/s3rp/cmd/s3rp

dist:
	goreleaser build --snapshot --clean

# HOME points weaver's registry cache at a writable place: the image's own
# home belongs to its uid, not to the one the container runs as.
WEAVER ?= docker run --rm -u $(shell id -u):$(shell id -g) -e HOME=/tmp -v $(CURDIR):/work -w /work otel/weaver:v0.26.1@sha256:9094862c0ab261bdbcb079bb981f9a573b3659b130a6d2ab8616eca6ba37aaec
SEMCONV = s3gw/semconv

# Checks the metrics registry and regenerates the Go constants and
# docs/metrics.md from it.
semconv:
	$(WEAVER) registry check -r $(SEMCONV)/model --future -p $(SEMCONV)/policies
	$(WEAVER) registry generate -r $(SEMCONV)/model -t $(SEMCONV)/templates --skip-policies go $(SEMCONV)
	$(WEAVER) registry generate -r $(SEMCONV)/model -t $(SEMCONV)/templates --skip-policies markdown docs
	gofmt -w $(SEMCONV)/semconv_gen.go
