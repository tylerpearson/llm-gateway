// Command gatewayctl is the admin CLI for the gateway config plane: applying
// migrations and seeding teams and virtual keys. Connection details come from
// the MYSQL_DSN environment variable (overridable with --dsn).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tylerpearson/llm-gateway/internal/auth"
	"github.com/tylerpearson/llm-gateway/internal/store/mysql"
)

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
	_ = fs.Parse(args)

	resolved, err := dsnFromEnv(*dsn)
	if err != nil {
		return err
	}
	if err := mysql.Migrate(resolved); err != nil {
		return err
	}
	fmt.Println("migrations applied")
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
	default:
		return fmt.Errorf("usage: gatewayctl key <create|list>")
	}
}
