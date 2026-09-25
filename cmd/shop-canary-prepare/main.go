// Command shop-canary-prepare computes the Champion Shop Phase 2C.2 canary preparation
// (docs/SHOP_DELIVERY_PHASE2C2.md) for one Nitrado service. It is READ-ONLY: it runs Phase 2C.1
// discovery (GET requests and signed downloads only), downloads cfggameplay.json, and prints the
// proposed patch (hashes and exact diff), the upload sequence and the gates. It never uploads,
// never requests an upload token and never restarts anything. It prints no token, signed URL,
// credential or physical account path.
//
//	NITRADO_TOKEN=... go run ./cmd/shop-canary-prepare -service <id> -org <orgID> -installation <id> -game-server <id>
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/canary"
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
	fixLosst := flag.Bool("correct-losst", true, "correct a present custom/The_Losst_City.json reference")
	reAddLost := flag.Bool("readd-lost-city", false, "owner option: add custom/The_Lost_City.json when absent")
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
	if rep.MissionDir == "" {
		return errors.New("the mission folder was not verified")
	}
	path, err := capability.SafePath(rep.FileRoot, rep.MissionDir+"/"+canary.ConfigRelPath)
	if err != nil {
		return err
	}
	raw, err := client.ReadLog(ctx, *service, path)
	if err != nil {
		return errors.New("cfggameplay.json could not be downloaded")
	}
	p, err := canary.ProposePatch(raw, canary.PatchOptions{CorrectLosstSpelling: *fixLosst, ReAddLostCity: *reAddLost})
	if err != nil {
		return err
	}
	fmt.Println("mission:", rep.MissionPath)
	fmt.Printf("current:  %s  %s  %d bytes\n", p.Path, p.CurrentSHA256, p.CurrentBytes)
	fmt.Printf("proposed: %s  %s  %d bytes\n", p.Path, p.ProposedSHA256, p.ProposedBytes)
	fmt.Printf("objectSpawnersArr: %q -> %q\n", p.Before, p.After)
	fmt.Println("changes:", strings.Join(p.Changes, "; "))
	fmt.Println("diff (changed lines and context):")
	for _, l := range strings.Split(p.Diff, "\n") {
		if strings.HasPrefix(l, "+ ") || strings.HasPrefix(l, "- ") || strings.Contains(l, "objectSpawnersArr") {
			fmt.Println("  " + l)
		}
	}
	for _, a := range p.AffectedFiles {
		fmt.Printf("affected: %s %s (gate %s)\n", a.Change, a.Path, a.Gate)
	}
	fmt.Println("write capability:", canary.WriteCapability)
	for _, s := range canary.UploadSequence(p.Path, p.CurrentSHA256, p.ProposedSHA256, canary.ConfigBackupPath(p.CurrentSHA256)) {
		fmt.Printf("  %d %-6s %s | abort: %s\n", s.N, s.Kind, s.Action, s.AbortIf)
	}
	return nil
}
