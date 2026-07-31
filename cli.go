package s3rp

import (
	"github.com/alecthomas/kong"
)

type CLI struct {
	Config   string           `help:"config file path" default:"s3rp.yaml" env:"S3RP_CONFIG"`
	Listen   string           `help:"listen address (overrides config)" env:"S3RP_LISTEN"`
	LogLevel string           `help:"log level" default:"info" enum:"debug,info,warn,error" env:"S3RP_LOG_LEVEL"`
	Version  kong.VersionFlag `help:"show version"`
}

func parseCLI() (*CLI, error) {
	var cli CLI
	kong.Parse(&cli,
		kong.Name("s3rp"),
		kong.Description("S3 API reverse proxy with SigV4 re-signing"),
		kong.Vars{"version": Version},
	)
	return &cli, nil
}
