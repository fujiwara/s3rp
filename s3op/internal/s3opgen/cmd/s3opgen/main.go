// Command s3opgen writes the s3op catalog (go generate in s3op).
package main

import (
	"log"
	"os"

	"github.com/fujiwara/s3rp/s3op/internal/s3opgen"
)

func main() {
	path, src, err := s3opgen.Generate(".")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(path, src, 0o644); err != nil {
		log.Fatal(err)
	}
}
