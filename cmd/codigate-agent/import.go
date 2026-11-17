package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/codigate/agent/importer"
)

const importUsage = `Usage: codigate-agent import cloudflared [flags] [cloudflared-config.yml]
       codigate-agent import cloudflared -cf [flags]

Converts a cloudflared config.yml (default ~/.cloudflared/config.yml) into a codigate-agent
config. With -cf it reads a remotely managed tunnel (cloudflared run with --token) from the
Cloudflare API instead: set CLOUDFLARE_API_TOKEN to a token with Account > Cloudflare Tunnel >
Read, and TUNNEL_TOKEN to the tunnel token (or pass -cf-tunnel and -cf-account). The tunnel
token's secret is not used or sent anywhere.

With -api it also creates the apps (and, without -agent, a new agent) in your workspace
through the API; set CODIGATE_API_TOKEN to an admin API token.

With -cf-access it also reads your Cloudflare Access applications (the API token then needs
Account > Access: Apps and Policies > Read too) and shows which access rules carry over. With
-api and -idp it sets them on the new apps. Rules Codigate can't express without letting more
people in (require, exclude, deny, bypass) are never imported: those apps are left for you.

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
	fromCF := fs.Bool("cf", false, "read a remotely managed tunnel's rules from the Cloudflare API")
	cfTunnel := fs.String("cf-tunnel", "", "with -cf: tunnel id or tunnel token (default: $TUNNEL_TOKEN)")
	cfAccount := fs.String("cf-account", "", "with -cf and a tunnel id: Cloudflare account id")
	cfAccess := fs.Bool("cf-access", false, "map Cloudflare Access policies onto the apps (needs the account: TUNNEL_TOKEN or -cf-account)")
	idpID := fs.String("idp", "", "with -api and -cf-access: identity provider (idp_…) the imported access rules use")
	cfAPI := fs.String("cf-api", "https://api.cloudflare.com/client/v4", "with -cf: Cloudflare API URL")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 1 || (*fromCF && fs.NArg() > 0) {
		fs.Usage()
		return 2
	}
	var data []byte
	var err error
	if *fromCF {
		if data, err = readCloudflare(*cfAPI, or(*cfTunnel, os.Getenv("TUNNEL_TOKEN")), *cfAccount, os.Getenv("CLOUDFLARE_API_TOKEN")); err != nil {
			fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
			return 1
		}
	} else {
		src := fs.Arg(0)
		if src == "" {
			home, _ := os.UserHomeDir()
			src = filepath.Join(home, ".cloudflared", "config.yml")
		}
		if data, err = os.ReadFile(src); err != nil {
			fmt.Fprintf(stderr, "codigate-agent: %v\n", err)
			return 2
		}
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

	var mappings []importer.AccessMapping
	if *cfAccess {
		if mappings, err = readAccess(*cfAPI, or(*cfTunnel, os.Getenv("TUNNEL_TOKEN")), *cfAccount, os.Getenv("CLOUDFLARE_API_TOKEN"), res.Apps); err != nil {
			fmt.Fprintf(stderr, "codigate-agent: Cloudflare Access: %v\n", err)
			return 1
		}
		printAccess(mappings, stderr)
	}

	if *apiURL == "" {
		if len(res.Apps) > 0 {
			fmt.Fprintf(stderr, "next: create these apps in the dashboard, or rerun with -api: %v\n", res.Apps)
		}
		return 0
	}
	if *cfAccess && *idpID == "" {
		fmt.Fprintln(stderr, "note: pass -idp idp_… to set the imported access rules on the new apps")
	}
	api := &importer.API{BaseURL: *apiURL, Token: os.Getenv("CODIGATE_API_TOKEN")}
	created, code := createApps(res, api, *agentID, or(*agentName, res.Tunnel, "imported-cloudflared"), *writeToken, *dryRun, stderr)
	if *cfAccess && *idpID != "" && !*dryRun && len(created) > 0 {
		if applyAccess(api, created, mappings, *idpID, stderr) > 0 {
			code = 1
		}
	}
	if len(created) > 0 || *dryRun {
		fmt.Fprintln(stderr, "next: check who may access each app (dashboard → Apps → Access); apps without access are not served")
	}
	return code
}

// readAccess fetches the account's Access applications and maps them onto hostnames (P2-03).
func readAccess(apiURL, ref, account, token string, hostnames []string) ([]importer.AccessMapping, error) {
	if token == "" {
		return nil, errors.New("needs CLOUDFLARE_API_TOKEN (Account > Access: Apps and Policies > Read)")
	}
	if ref != "" {
		acc, _, err := importer.ParseTunnelRef(ref)
		if err != nil {
			return nil, fmt.Errorf("-cf-tunnel: %w", err)
		}
		if acc != "" && account == "" {
			account = acc
		}
	}
	if account == "" {
		return nil, errors.New("needs the Cloudflare account: pass -cf-account or set TUNNEL_TOKEN")
	}
	account = strings.ToLower(account)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cf := &importer.CloudflareAPI{BaseURL: apiURL, Token: token}
	apps, err := cf.AccessApps(ctx, account)
	if err != nil {
		return nil, err
	}
	cache := map[string]*importer.CFAccessPolicy{}
	return importer.MapAccess(apps, hostnames, func(id string) (*importer.CFAccessPolicy, error) {
		if g, ok := cache[id]; ok {
			return g, nil
		}
		g, err := cf.AccessGroup(ctx, account, id)
		if err == nil {
			cache[id] = g
		}
		return g, err
	}), nil
}

func printAccess(mappings []importer.AccessMapping, w io.Writer) {
	for _, m := range mappings {
		switch {
		case m.Policy != nil:
			fmt.Fprintf(w, "access %s (from %q): %s\n", m.Hostname, m.App, describePolicy(m.Policy))
		default:
			fmt.Fprintf(w, "access %s: not imported\n", m.Hostname)
		}
		for _, n := range m.Notes {
			fmt.Fprintf(w, "  %s\n", n)
		}
	}
}

func describePolicy(p *importer.Policy) string {
	var parts []string
	if p.Everyone {
		parts = append(parts, "everyone who can log in")
	}
	for label, list := range map[string][]string{"emails": p.Emails, "domains": p.EmailDomains, "groups": p.Groups} {
		if len(list) > 0 {
			parts = append(parts, label+" "+strings.Join(list, ", "))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func applyAccess(api *importer.API, created map[string]string, mappings []importer.AccessMapping, idpID string, stderr io.Writer) (failed int) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for _, m := range mappings {
		id, ok := created[m.Hostname]
		if !ok || m.Policy == nil {
			continue
		}
		if err := api.ProtectApp(ctx, id, idpID, m.Policy); err != nil {
			failed++
			fmt.Fprintf(stderr, "access %s: %v\n", m.Hostname, err)
			continue
		}
		fmt.Fprintf(stderr, "access %s: set (%s)\n", m.Hostname, describePolicy(m.Policy))
	}
	return failed
}

// readCloudflare fetches a remotely managed tunnel's configuration (P2-16).
func readCloudflare(apiURL, ref, account, token string) ([]byte, error) {
	if ref == "" {
		return nil, errors.New("-cf needs the tunnel: set TUNNEL_TOKEN or pass -cf-tunnel")
	}
	if token == "" {
		return nil, errors.New("-cf needs CLOUDFLARE_API_TOKEN (Account > Cloudflare Tunnel > Read)")
	}
	acc, tunnel, err := importer.ParseTunnelRef(ref)
	if err != nil {
		return nil, fmt.Errorf("-cf-tunnel: %w", err)
	}
	if acc == "" {
		if acc = strings.ToLower(account); acc == "" {
			return nil, errors.New("-cf-tunnel is a tunnel id; pass -cf-account too (or the tunnel token)")
		}
	} else if account != "" && !strings.EqualFold(account, acc) {
		return nil, errors.New("-cf-account differs from the account in the tunnel token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return (&importer.CloudflareAPI{BaseURL: apiURL, Token: token}).TunnelConfig(ctx, acc, tunnel)
}

// createApps returns the new apps' ids by hostname and the exit code.
func createApps(res *importer.Result, api *importer.API, agentID, agentName, tokenPath string, dryRun bool, stderr io.Writer) (map[string]string, int) {
	created := map[string]string{}
	if api.Token == "" {
		fmt.Fprintln(stderr, "codigate-agent: set CODIGATE_API_TOKEN to an admin API token to use -api")
		return created, 2
	}
	if dryRun {
		if agentID == "" {
			fmt.Fprintf(stderr, "would create agent %q and save its token to %s\n", agentName, tokenPath)
		}
		for _, h := range res.Apps {
			fmt.Fprintf(stderr, "would create app %s\n", h)
		}
		return created, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if agentID == "" {
		// Refuse before creating anything, so a failed write can't orphan a token.
		if _, err := os.Stat(tokenPath); err == nil {
			fmt.Fprintf(stderr, "codigate-agent: %s exists; pass -agent or another -write-token\n", tokenPath)
			return created, 1
		}
		id, token, err := api.CreateAgent(ctx, agentName)
		if err != nil {
			fmt.Fprintf(stderr, "codigate-agent: creating the agent: %v\n", err)
			return created, 1
		}
		if err := writeNew(tokenPath, []byte(token+"\n"), 0o600, false); err != nil {
			fmt.Fprintf(stderr, "codigate-agent: agent %s created, but saving its token failed (%v); rotate it in the dashboard\n", id, err)
			return created, 1
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
		created[h] = id
		fmt.Fprintf(stderr, "created app %s (%s)\n", h, id)
	}
	if failed > 0 {
		return created, 1
	}
	return created, 0
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
