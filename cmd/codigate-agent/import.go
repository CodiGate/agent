package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/codigate/agent/importer"
)

const importUsage = `Usage: codigate-agent import cloudflared [flags] [cloudflared-config.yml]

Converts a cloudflared config.yml (default ~/.cloudflared/config.yml) into a codigate-agent
config. With -api it also creates the apps (and, without -agent, a new agent) in your workspace
through the API; set CODIGATE_API_TOKEN to an admin API token.

Flags:
`

// runImport implements "codigate-agent import cloudflared". It returns the exit code.
func runImport(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "cloudflared" {
		fmt.Fprint(stderr, importUsage)
		return 2
	}
	fs := flag.NewFlagSet("import cloudflared", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, importUsage); fs.PrintDefaults() }
	out := fs.String("out", "codigate.yml", `where to write the agent config ("-" for stdout)`)
	force := fs.Bool("force", false, "overwrite -out if it exists")
	edge := fs.String("edge", "https://connect.codigate.nl", "edge connect URL for the agent config")
	tokenFile := fs.String("token-file", "/etc/codigate/token", "token_file path in the agent config")
	apiURL := fs.String("api", os.Getenv("CODIGATE_API_URL"), "control plane URL, e.g. https://app.codigate.nl; enables creating the apps")
	agentID := fs.String("agent", "", "existing agent id (agt_…) for the apps; default: create a new agent")
	agentName := fs.String("agent-name", "", "name of the new agent (default: the cloudflared tunnel name)")
	writeToken := fs.String("write-token", "codigate-token", "where to save a new agent's token (mode 0600)")
	dryRun := fs.Bool("dry-run", false, "with -api: show what would be created, change nothing")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	src := fs.Arg(0)
	if src == "" {
		home, _ := os.UserHomeDir()
		src = filepath.Join(home, ".cloudflared", "config.yml")
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}

	data, err := os.ReadFile(src)
	if err != nil {
		fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
		return 2
	}
	res, err := importer.Cloudflared(data, importer.Options{Edge: *edge, TokenFile: *tokenFile})
	if err != nil {
		fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
		return 1
	}
	for _, n := range res.Notes {
		fmt.Fprintln(stderr, n)
	}

	if *out == "-" {
		stdout.Write(res.Config)
	} else if err := writeNew(*out, res.Config, 0o644, *force); err != nil {
		fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
		return 1
	} else {
		fmt.Fprintf(stderr, "wrote %s (%d apps, %d warnings)\n", *out, len(res.Apps), res.Warnings())
	}

	if *apiURL == "" {
		if len(res.Apps) > 0 {
			fmt.Fprintf(stderr, "next: create these apps in the dashboard, or rerun with -api: %v\n", res.Apps)
		}
		return 0
	}
	return createApps(res, &importer.API{BaseURL: *apiURL, Token: os.Getenv("CODIGATE_API_TOKEN")}, *agentID,
		or(*agentName, res.Tunnel, "imported-cloudflared"), *writeToken, *dryRun, stderr)
}

func createApps(res *importer.Result, api *importer.API, agentID, agentName, tokenPath string, dryRun bool, stderr io.Writer) int {
	if api.Token == "" {
		fmt.Fprintln(stderr, "codigate-agent: set CODIGATE_API_TOKEN to an admin API token to use -api")
		return 2
	}
	if dryRun {
		if agentID == "" {
			fmt.Fprintf(stderr, "would create agent %q and save its token to %s\n", agentName, tokenPath)
		}
		for _, h := range res.Apps {
			fmt.Fprintf(stderr, "would create app %s\n", h)
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if agentID == "" {
		// Refuse before creating anything, so a failed write can't orphan a token.
		if _, err := os.Stat(tokenPath); err == nil {
			fmt.Fprintf(stderr, "codigate-agent: %s exists; pass -agent or another -write-token\n", tokenPath)
			return 1
		}
		id, token, err := api.CreateAgent(ctx, agentName)
		if err != nil {
			fmt.Fprintf(stderr, "codigate-agent: creating the agent: %v\n", err)
			return 1
		}
		if err := writeNew(tokenPath, []byte(token+"\n"), 0o600, false); err != nil {
			fmt.Fprintf(stderr, "codigate-agent: agent %s created, but saving its token failed (%v); rotate it in the dashboard\n", id, err)
			return 1
		}
		fmt.Fprintf(stderr, "created agent %s; token saved to %s (copy it to the token_file path)\n", id, tokenPath)
		agentID = id
	}

	failed := 0
	for _, h := range res.Apps {
		id, err := api.CreateApp(ctx, importer.AppName(h), h, agentID)
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "app %s: %v\n", h, err)
			continue
		}
		fmt.Fprintf(stderr, "created app %s (%s)\n", h, id)
	}
	fmt.Fprintln(stderr, "next: set who may access each app (dashboard → Apps → Access); apps are not served until then")
	if failed > 0 {
		return 1
	}
	return 0
}

// writeNew writes a file, refusing to overwrite unless force is set.
func writeNew(path string, data []byte, mode os.FileMode, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, mode)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists (use -force to overwrite)", path)
	}
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func or(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
