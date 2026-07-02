// Command gatewayctl is the admin CLI for the gateway config plane: applying
// migrations and seeding teams and virtual keys. Connection details come from
// the MYSQL_DSN environment variable (overridable with --dsn).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/store/clickhouse"
	"github.com/tylerpearson/llm-gateway/internal/store/mysql"
)

// actor identifies who performed an administrative action for the audit log.
func actor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "gatewayctl"
}

// auditRecorder is the subset of the store needed to write audit entries,
// declared locally so the warning path is unit-testable with a fake.
type auditRecorder interface {
	RecordAudit(ctx context.Context, actor, action, target, details string) error
}

// recordAudit writes an audit entry and warns on w when the write fails.
// The admin action itself has already succeeded at this point, so the
// failure is surfaced but does not change the exit status.
func recordAudit(c context.Context, s auditRecorder, w io.Writer, action, target, details string) {
	if err := s.RecordAudit(c, actor(), action, target, details); err != nil {
		_, _ = fmt.Fprintf(w, "warning: %s succeeded but audit log write failed: %v\n", action, err)
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "migrate":
		err = cmdMigrate(os.Args[2:])
	case "team":
		err = cmdTeam(os.Args[2:])
	case "key":
		err = cmdKey(os.Args[2:])
	case "audit":
		err = cmdAudit(os.Args[2:])
	case "version":
		fmt.Println("gatewayctl v0 (P2)")
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gatewayctl <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  migrate                 apply pending MySQL migrations")
	fmt.Fprintln(os.Stderr, "  team create <name>      create a team")
	fmt.Fprintln(os.Stderr, "  team list               list teams")
	fmt.Fprintln(os.Stderr, "  key create --team <id> --name <name> [--alias <alias>]")
	fmt.Fprintln(os.Stderr, "  key list --team <id>    list a team's keys")
	fmt.Fprintln(os.Stderr, "  key disable --id <id>   disable a key")
	fmt.Fprintln(os.Stderr, "  audit [--limit N]       show recent audit log entries")
	fmt.Fprintln(os.Stderr, "  version                 print version")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "MYSQL_DSN must be set (or pass --dsn).")
}

func dsnFromEnv(flagDSN string) (string, error) {
	if flagDSN != "" {
		return flagDSN, nil
	}
	if v := os.Getenv("MYSQL_DSN"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("MYSQL_DSN not set and --dsn not provided")
}

func openStore(flagDSN string) (*mysql.Store, error) {
	dsn, err := dsnFromEnv(flagDSN)
	if err != nil {
		return nil, err
	}
	return mysql.Open(dsn)
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dsn := fs.String("dsn", "", "MySQL DSN (defaults to MYSQL_DSN env)")
	chDSN := fs.String("clickhouse-dsn", "", "ClickHouse DSN (defaults to CLICKHOUSE_DSN env)")
	_ = fs.Parse(args)

	resolved, err := dsnFromEnv(*dsn)
	if err != nil {
		return err
	}
	if err := mysql.Migrate(resolved); err != nil {
		return err
	}
	fmt.Println("mysql migrations applied")

	// ClickHouse is optional; migrate it when a DSN is configured.
	ch := *chDSN
	if ch == "" {
		ch = os.Getenv("CLICKHOUSE_DSN")
	}
	if ch != "" {
		if err := clickhouse.Migrate(ch); err != nil {
			return fmt.Errorf("clickhouse: %w", err)
		}
		fmt.Println("clickhouse migrations applied")
	}
	return nil
}

func cmdTeam(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gatewayctl team <create|list>")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("team create", flag.ExitOnError)
		dsn := fs.String("dsn", "", "MySQL DSN (defaults to MYSQL_DSN env)")
		_ = fs.Parse(args[1:])
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: gatewayctl team create <name>")
		}
		name := fs.Arg(0)

		s, err := openStore(*dsn)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()

		c, cancel := ctx()
		defer cancel()
		t, err := s.CreateTeam(c, name)
		if err != nil {
			return err
		}
		recordAudit(c, s, os.Stderr, "team.create", t.ID, "name="+t.Name)
		fmt.Printf("created team\n  id:   %s\n  name: %s\n", t.ID, t.Name)
		return nil
	case "list":
		fs := flag.NewFlagSet("team list", flag.ExitOnError)
		dsn := fs.String("dsn", "", "MySQL DSN (defaults to MYSQL_DSN env)")
		_ = fs.Parse(args[1:])

		s, err := openStore(*dsn)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()

		c, cancel := ctx()
		defer cancel()
		teams, err := s.ListTeams(c)
		if err != nil {
			return err
		}
		for _, t := range teams {
			fmt.Printf("%s  %s\n", t.ID, t.Name)
		}
		return nil
	default:
		return fmt.Errorf("usage: gatewayctl team <create|list>")
	}
}

func cmdKey(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: gatewayctl key <create|list>")
	}
	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("key create", flag.ExitOnError)
		dsn := fs.String("dsn", "", "MySQL DSN (defaults to MYSQL_DSN env)")
		team := fs.String("team", "", "team id (required)")
		name := fs.String("name", "", "key name (required)")
		alias := fs.String("alias", "", "default routing alias (optional)")
		_ = fs.Parse(args[1:])
		if *team == "" || *name == "" {
			return fmt.Errorf("key create requires --team and --name")
		}

		s, err := openStore(*dsn)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()

		plaintext, hash := auth.GenerateKey()
		c, cancel := ctx()
		defer cancel()
		vk, err := s.CreateKey(c, *team, *name, hash, *alias)
		if err != nil {
			return err
		}
		recordAudit(c, s, os.Stderr, "key.create", vk.ID, "team="+vk.TeamID+" name="+vk.Name)
		fmt.Printf("created virtual key\n  id:    %s\n  team:  %s\n  name:  %s\n", vk.ID, vk.TeamID, vk.Name)
		if vk.DefaultAlias != "" {
			fmt.Printf("  alias: %s\n", vk.DefaultAlias)
		}
		fmt.Printf("\nAPI key (shown once, store it now):\n  %s\n", plaintext)
		return nil
	case "list":
		fs := flag.NewFlagSet("key list", flag.ExitOnError)
		dsn := fs.String("dsn", "", "MySQL DSN (defaults to MYSQL_DSN env)")
		team := fs.String("team", "", "team id (required)")
		_ = fs.Parse(args[1:])
		if *team == "" {
			return fmt.Errorf("key list requires --team")
		}

		s, err := openStore(*dsn)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()

		c, cancel := ctx()
		defer cancel()
		keys, err := s.ListKeys(c, *team)
		if err != nil {
			return err
		}
		for _, k := range keys {
			status := "active"
			if k.Disabled {
				status = "disabled"
			}
			fmt.Printf("%s  %-20s  %s  alias=%s\n", k.ID, k.Name, status, k.DefaultAlias)
		}
		return nil
	case "disable":
		fs := flag.NewFlagSet("key disable", flag.ExitOnError)
		dsn := fs.String("dsn", "", "MySQL DSN (defaults to MYSQL_DSN env)")
		id := fs.String("id", "", "key id (required)")
		_ = fs.Parse(args[1:])
		if *id == "" {
			return fmt.Errorf("key disable requires --id")
		}

		s, err := openStore(*dsn)
		if err != nil {
			return err
		}
		defer func() { _ = s.Close() }()

		c, cancel := ctx()
		defer cancel()
		if err := s.DisableKey(c, *id); err != nil {
			return err
		}
		recordAudit(c, s, os.Stderr, "key.disable", *id, "")
		fmt.Printf("disabled key %s\n", *id)
		return nil
	default:
		return fmt.Errorf("usage: gatewayctl key <create|list|disable>")
	}
}

func cmdAudit(args []string) error {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	dsn := fs.String("dsn", "", "MySQL DSN (defaults to MYSQL_DSN env)")
	limit := fs.Int("limit", 50, "max entries to show")
	_ = fs.Parse(args)

	s, err := openStore(*dsn)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	c, cancel := ctx()
	defer cancel()
	entries, err := s.ListAudit(c, *limit)
	if err != nil {
		return err
	}
	for _, e := range entries {
		fmt.Printf("%s  %-16s  %-12s  %s  %s\n",
			e.CreatedAt.Format(time.RFC3339), e.Actor, e.Action, e.Target, e.Details)
	}
	return nil
}
