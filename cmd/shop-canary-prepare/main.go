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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/nitradodelivery"
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
	fmt.Println("mission:", rep.MissionPath)
	fmt.Println("write capability:", canary.WriteCapability)

	// Gate A: the empty Champion file.
	empty, emptySHA := canary.EmptyArtifact()
	fmt.Printf("\n[Gate A] create %s = empty spawner file, %d bytes, SHA-256 %s\n", "champion/champion_shop_delivery.json", len(empty), emptySHA)
	artPath, err := capability.SafePath(rep.FileRoot, rep.MissionDir+"/champion/champion_shop_delivery.json")
	if err != nil {
		return err
	}
	if art, err := client.ReadLog(ctx, *service, artPath); err == nil {
		fmt.Println("  current Champion file: present, SHA-256", nitradodelivery.SHA256(art), "- reference precondition:", errText(canary.CheckReferencePrecondition(art, true)))
	} else {
		fmt.Println("  current Champion file: not readable (absent) - reference precondition:", errText(canary.CheckReferencePrecondition(nil, false)))
	}

	// Gate B: the reference patch (only after Gate A is verified).
	p, err := canary.ProposePatch(raw)
	if err != nil {
		return err
	}
	printPatch("[Gate B] Shop reference", p)
	for _, s := range canary.UploadSequence(p.Path, p.CurrentSHA256, p.ProposedSHA256, canary.ConfigBackupPath(p.CurrentSHA256)) {
		fmt.Printf("  %d %-6s %s | abort: %s\n", s.N, s.Kind, s.Action, s.AbortIf)
	}

	// Separate owner decision: The Lost City.
	lc, err := canary.ProposeLostCityRestore(raw)
	if err != nil {
		fmt.Println("\n[LOST_CITY] no change:", err)
	} else {
		printPatch("[LOST_CITY] optional, separate from the Shop", lc)
	}
	lcPath, err := capability.SafePath(rep.FileRoot, rep.MissionDir+"/"+canary.LostCityRelPath)
	if err != nil {
		return err
	}
	if b, err := client.ReadLog(ctx, *service, lcPath); err == nil {
		var f struct {
			Objects []json.RawMessage `json:"Objects"`
		}
		perr := json.Unmarshal(b, &f)
		fmt.Printf("  %s: %d bytes, SHA-256 %s, parses as a spawner file: %v, objects: %d\n", canary.LostCityRelPath, len(b), nitradodelivery.SHA256(b), perr == nil, len(f.Objects))
	} else {
		fmt.Println(" ", canary.LostCityRelPath, "could not be downloaded")
	}
	return nil
}

func printPatch(title string, p canary.ConfigPatch) {
	fmt.Printf("\n%s (%s)\n", title, p.Purpose)
	fmt.Printf("  current:  %s  %s  %d bytes\n", p.Path, p.CurrentSHA256, p.CurrentBytes)
	fmt.Printf("  proposed: %s  %s  %d bytes\n", p.Path, p.ProposedSHA256, p.ProposedBytes)
	fmt.Printf("  objectSpawnersArr: %q -> %q\n", p.Before, p.After)
	fmt.Println("  changes:", strings.Join(p.Changes, "; "))
	for _, l := range strings.Split(p.Diff, "\n") {
		if strings.HasPrefix(l, "+ ") || strings.HasPrefix(l, "- ") || strings.Contains(l, "objectSpawnersArr") {
			fmt.Println("    " + l)
		}
	}
	for _, c := range p.Preconditions {
		fmt.Println("  precondition:", c)
	}
}

func errText(err error) string {
	if err == nil {
		return "OK"
	}
	return err.Error()
}
