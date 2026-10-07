package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/luisgf/agent-sandbox-platform/cli/internal/client"
)

func apikeyCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "apikey subcommand required (create|list|revoke|rotate)")
		return 2
	}
	switch args[0] {
	case "create":
		return cmdAPIKeyCreate(args[1:], stdout, stderr)
	case "list", "ls":
		return cmdAPIKeyList(args[1:], stdout, stderr)
	case "revoke":
		return cmdAPIKeyByID("revoke", args[1:], stdout, stderr)
	case "rotate":
		return cmdAPIKeyByID("rotate", args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printRootUsage(stderr)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown apikey subcommand %q\n", args[0])
		return 2
	}
}

// cmdAPIKeyCreate prints the new secret on stdout, alone, so that
// `KEY=$(asp apikey create --name ci)` captures it, and says on stderr that it
// will not be shown again.
func cmdAPIKeyCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apikey create", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	name := fs.String("name", "", "key name, unique within the tenant (letters, digits, . _ -)")
	scope := fs.String("scope", "", "tenant (default) or platform (every tenant and the node routes; platform keys only)")
	ttl := fs.String("ttl", "", "how long the key lasts, e.g. 720h or 30d (default: no expiry)")
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	if strings.TrimSpace(*name) == "" {
		fmt.Fprintln(stderr, "usage: asp apikey create --name N [--tenant T] [--scope tenant|platform] [--ttl 30d] [--json]")
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	key, err := c.CreateAPIKey(context.Background(), clientCreateInput(g.tenant, *name, *scope, *ttl))
	if err != nil {
		fmt.Fprintf(stderr, "apikey create: %v\n", err)
		return 1
	}
	if g.jsonOut {
		return writeJSON(stdout, key)
	}
	fmt.Fprintln(stdout, key.Secret)
	fmt.Fprintf(stderr, "key %s (%s, tenant %s, scope %s)%s\nThis is the only time the secret is shown.\n",
		key.ID, key.Name, key.TenantID, key.Scope, expiryNote(key.ExpiresAt))
	return 0
}

func cmdAPIKeyList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apikey list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	if _, err := parseInterspersed(fs, args); err != nil {
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	keys, err := c.ListAPIKeys(context.Background(), g.tenant)
	if err != nil {
		fmt.Fprintf(stderr, "apikey list: %v\n", err)
		return 1
	}
	if g.jsonOut {
		return writeJSON(stdout, map[string]any{"keys": keys})
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTENANT\tNAME\tSCOPE\tPREFIX\tSTATE\tEXPIRES\tLAST USED")
	now := time.Now()
	for _, k := range keys {
		state := "active"
		switch {
		case k.RevokedAt != nil:
			state = "revoked"
		case k.ExpiresAt != nil && k.ExpiresAt.Before(now):
			state = "expired"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.TenantID, k.Name, k.Scope, k.KeyPrefix, state,
			timeOrDash(k.ExpiresAt), timeOrDash(k.LastUsedAt))
	}
	_ = tw.Flush()
	return 0
}

func cmdAPIKeyByID(action string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("apikey "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var g globalFlags
	addGlobalFlags(fs, &g)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "usage: asp apikey %s <id>\n", action)
		return 2
	}
	c, code := mustClient(g, stderr)
	if c == nil {
		return code
	}
	ctx := context.Background()
	if action == "revoke" {
		key, err := c.RevokeAPIKey(ctx, pos[0])
		if err != nil {
			fmt.Fprintf(stderr, "apikey revoke: %v\n", err)
			return 1
		}
		if g.jsonOut {
			return writeJSON(stdout, key)
		}
		fmt.Fprintf(stdout, "key %s (%s) revoked\n", key.ID, key.Name)
		return 0
	}
	key, err := c.RotateAPIKey(ctx, pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "apikey rotate: %v\n", err)
		return 1
	}
	if g.jsonOut {
		return writeJSON(stdout, key)
	}
	fmt.Fprintln(stdout, key.Secret)
	fmt.Fprintf(stderr, "key %s (%s) has a new secret; the old one no longer works.\nThis is the only time the secret is shown.\n", key.ID, key.Name)
	return 0
}

func timeOrDash(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func expiryNote(t *time.Time) string {
	if t == nil {
		return ", no expiry"
	}
	return ", expires " + t.Local().Format(time.RFC3339)
}

func clientCreateInput(tenant, name, scope, ttl string) client.CreateAPIKeyInput {
	return client.CreateAPIKeyInput{TenantID: strings.TrimSpace(tenant), Name: strings.TrimSpace(name), Scope: strings.TrimSpace(scope), TTL: strings.TrimSpace(ttl)}
}
