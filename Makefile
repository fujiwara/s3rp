.PHONY: all clean test install dist semconv live-check-start live-check-stop

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

LIVE_CHECK_OTLP_PORT ?= 4317
LIVE_CHECK_ADMIN_PORT ?= 4320

# Starts weaver live-check as an OTLP/gRPC receiver on LIVE_CHECK_OTLP_PORT.
# Point the integration suite at it (OTEL_EXPORTER_OTLP_ENDPOINT, protocol
# grpc), then live-check-stop reports and fails on a registry violation.
live-check-start:
	-docker rm -f s3rp-live-check >/dev/null 2>&1
	docker run -d --name s3rp-live-check -u $(shell id -u):$(shell id -g) -e HOME=/tmp \
		-p $(LIVE_CHECK_OTLP_PORT):4317 -p $(LIVE_CHECK_ADMIN_PORT):4320 -v $(CURDIR):/work -w /work \
		$(WEAVER_IMAGE) registry live-check -r $(SEMCONV)/model --input-source otlp \
		--otlp-grpc-address 0.0.0.0 --otlp-grpc-port 4317 --admin-port 4320 \
		--inactivity-timeout 900 --format json -o live-check
	@for i in $$(seq 1 60); do \
		docker logs s3rp-live-check 2>&1 | grep -q "OTLP receiver will stop" && exit 0; \
		sleep 2; \
	done; docker logs s3rp-live-check; exit 1

# Stops the receiver; the report is live-check/live_check.json.
live-check-stop:
	curl -fsS -X POST http://localhost:$(LIVE_CHECK_ADMIN_PORT)/stop >/dev/null
	@code=$$(docker wait s3rp-live-check); docker logs s3rp-live-check; docker rm s3rp-live-check >/dev/null; \
		jq -r '[.. | objects | select(.level? == "violation") | .message] | unique | .[]' live-check/live_check.json; \
		exit $$code
