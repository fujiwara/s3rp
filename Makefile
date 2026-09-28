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

SEMCONV = s3gw/semconv
# the version lives in a Dockerfile so dependabot keeps it current
WEAVER_IMAGE := $(shell sed -n 's/^FROM //p' $(SEMCONV)/Dockerfile)
# HOME points weaver's registry cache at a writable place: the image's own
# home belongs to its uid, not to the one the container runs as.
WEAVER ?= docker run --rm -u $(shell id -u):$(shell id -g) -e HOME=/tmp -v $(CURDIR):/work -w /work $(WEAVER_IMAGE)

# Checks the metrics registry and regenerates the Go constants and
# docs/metrics.md from it.
semconv:
	$(WEAVER) registry check -r $(SEMCONV)/model --future -p $(SEMCONV)/policies
	$(WEAVER) registry generate -r $(SEMCONV)/model -t $(SEMCONV)/templates --skip-policies go $(SEMCONV)
	$(WEAVER) registry generate -r $(SEMCONV)/model -t $(SEMCONV)/templates --skip-policies markdown docs
	gofmt -w $(SEMCONV)/semconv_gen.go
