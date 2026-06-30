// Command gatewayctl is the admin CLI for managing virtual keys, teams,
// budgets, and routing rules. It is scaffolded in P0 and gains real
// subcommands in the virtual keys and budgets phases.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("gatewayctl (scaffold)")
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gatewayctl <command>")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  version   print the CLI version")
	fmt.Fprintln(os.Stderr, "  help      show this help")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "key, team, budget, and rule subcommands arrive in later phases")
}
