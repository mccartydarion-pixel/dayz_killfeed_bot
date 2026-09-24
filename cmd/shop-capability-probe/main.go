// Command shop-capability-probe runs Champion Shop Phase 2C.1 capability discovery
// (docs/SHOP_DELIVERY_PHASE2C1.md) against one Nitrado service. It is READ-ONLY: it uses only
// internal/shop/capability's Reader (GET requests and signed downloads) and prints the sanitized
// JSON report - never the token, a signed URL, a credential, or a configuration file's contents.
//
//	NITRADO_TOKEN=... go run ./cmd/shop-capability-probe -service <id> -org <orgID> -installation <id> -game-server <id>
//
// The binding flags must be the installation's stored binding; discovery refuses a service that
// does not match it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}
}

func run() error {
	service := flag.String("service", "", "Nitrado service ID (the installation's bound service)")
	org := flag.Int64("org", 0, "organization ID")
	inst := flag.Int64("installation", 0, "installation ID")
	gameServer := flag.Int64("game-server", 0, "game_servers.id bound to the installation")
	timeout := flag.Duration("timeout", 90*time.Second, "overall timeout")
	flag.Parse()
	token := os.Getenv("NITRADO_TOKEN")
	if token == "" {
		return errors.New("NITRADO_TOKEN is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := nitrado.NewClient(nitrado.DefaultBaseURL, token, nil)
	rep, err := capability.Discover(ctx, client, capability.Binding{OrganizationID: *org, InstallationID: *inst, GameServerID: *gameServer, NitradoServiceID: *service})
	if err != nil {
		return err
	}
	rep.MissionDir = "" // the physical path carries the account name; the canonical MissionPath is kept
	rep.FileRoot = ""
	out, _ := json.MarshalIndent(rep, "", "  ")
	fmt.Println(string(out))
	return nil
}
