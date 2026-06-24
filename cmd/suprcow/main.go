// Command suprcow runs on-demand, per-PR preview environments
// (Compose On-demand Workspaces).
//
// Usage:
//
//	suprcow validate [preview.yml]   validate a project config (default ./preview.yml)
//	suprcow serve                    run the daemon (not yet implemented)
//	suprcow version                  print version
package main

import (
	"fmt"
	"os"

	"github.com/hauxir/suprcow/internal/config"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "validate":
		os.Exit(cmdValidate(os.Args[2:]))
	case "serve":
		os.Exit(cmdServe(os.Args[2:]))
	case "version", "-v", "--version":
		fmt.Println("suprcow", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func cmdValidate(args []string) int {
	path := "preview.yml"
	if len(args) > 0 {
		path = args[0]
	}
	c, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("%s is valid: repo=%s, %d exposed service(s), max_running=%d, idle_timeout=%s\n",
		path, c.Repo, len(c.Expose), c.MaxRunning, c.IdleTimeout.Duration())
	for _, e := range c.Expose {
		fmt.Printf("  - %s -> %s (:%d)\n", e.Service, e.Subdomain, e.Port)
	}
	return 0
}

func usage() {
	fmt.Fprint(os.Stderr, `suprcow - on-demand per-PR preview environments

Usage:
  suprcow validate [preview.yml]   validate a project config (default ./preview.yml)
  suprcow serve                    run the daemon
  suprcow version                  print version
`)
}
