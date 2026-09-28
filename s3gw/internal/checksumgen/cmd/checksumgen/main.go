// Command checksumgen writes the checksum conversions (go generate in s3gw).
package main

import (
	"log"
	"os"

	"github.com/fujiwara/s3rp/s3gw/internal/checksumgen"
)

func main() {
	files, err := checksumgen.Files()
	if err != nil {
		log.Fatal(err)
	}
	for path, src := range files {
		if err := os.WriteFile(path, src, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
