.PHONY: clean test install dist

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
