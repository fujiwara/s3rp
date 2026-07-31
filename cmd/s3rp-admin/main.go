// s3rp-admin is the write-side tooling for the s3rp database: schema
// migration and importing a YAML config. It is a separate binary from the
// proxy so that the proxy image never contains write code or credentials.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"

	"github.com/alecthomas/kong"
	app "github.com/fujiwara/s3rp"
	"github.com/fujiwara/s3rp/db"
	_ "modernc.org/sqlite"
)

type CLI struct {
	Migrate MigrateCmd       `cmd:"" help:"apply the schema to the database"`
	Import  ImportCmd        `cmd:"" help:"import a YAML config into the database"`
	Version kong.VersionFlag `help:"show version"`

	DSN string `required:"" help:"sqlite DSN (e.g. s3rp.db)" env:"S3RP_ADMIN_DSN"`
}

type MigrateCmd struct{}

func (c *MigrateCmd) Run(ctx context.Context, cli *CLI) error {
	sqldb, err := openRW(cli.DSN)
	if err != nil {
		return err
	}
	defer sqldb.Close()
	if err := db.Migrate(ctx, sqldb); err != nil {
		return err
	}
	slog.InfoContext(ctx, "schema applied", "dsn", cli.DSN)
	return nil
}

type ImportCmd struct {
	Config string `default:"s3rp.yaml" help:"config file path (tenants form)"`
}

func (c *ImportCmd) Run(ctx context.Context, cli *CLI) error {
	cfg, err := app.LoadConfig(c.Config)
	if err != nil {
		return err
	}
	sqldb, err := openRW(cli.DSN)
	if err != nil {
		return err
	}
	defer sqldb.Close()
	if err := db.Import(ctx, sqldb, cfg); err != nil {
		return err
	}
	slog.InfoContext(ctx, "config imported", "config", c.Config, "dsn", cli.DSN)
	return nil
}

func openRW(dsn string) (*sql.DB, error) {
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := sqldb.Ping(); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	return sqldb, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var cli CLI
	k := kong.Parse(&cli,
		kong.Name("s3rp-admin"),
		kong.Description("write-side admin tooling for the s3rp database"),
		kong.Vars{"version": app.Version},
		kong.BindTo(ctx, (*context.Context)(nil)),
	)
	if err := k.Run(&cli); err != nil {
		slog.ErrorContext(ctx, err.Error())
		os.Exit(1)
	}
}
