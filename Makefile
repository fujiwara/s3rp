.PHONY: all clean test install dist gen

all: s3rp s3rp-admin

s3rp: go.* *.go
	go build -o $@ ./cmd/s3rp/

s3rp-admin: go.* *.go db/*.go db/writedb/*.go
	go build -o $@ ./cmd/s3rp-admin/

clean:
	rm -rf s3rp s3rp-admin dist/

test:
	go test -race ./...

install:
	go install github.com/fujiwara/s3rp/cmd/s3rp
	go install github.com/fujiwara/s3rp/cmd/s3rp-admin

gen:
	cd db && sqlc generate

dist:
	goreleaser build --snapshot --clean
