// Command shop-mission-write is the guarded, single-file Nitrado write for the Champion Shop canary
// (docs/SHOP_GATE_A_UPLOAD.md). Without -execute it is READ-ONLY: it inspects the live server and
// prints the plan and its ID. With -execute -authorize <plan ID> it performs exactly that plan once,
// reads the file back and reports WRITTEN_VERIFIED, NOT_WRITTEN or UNCERTAIN. It never prints the
// Nitrado token, an upload token, a signed URL or the account's physical path.
//
// Only Gate A is active:
//
//	NITRADO_TOKEN=... go run ./cmd/shop-mission-write -operation gate-a-create-empty \
//	    -service 19806451 -org 1 -installation 11 -game-server 1 \
//	    -mission dayzps_missions/dayzOffline.chernarusplus -path champion/champion_shop_delivery.json \
//	    -expect-current absent -expect-config-sha256 <sha> -expect-spawners '["custom/The_Lost_City.json"]' \
//	    -expect-payload-sha256 328c4d64bb81bdbdddad2431a12b6197182f5fa5646bf16c7e0e8d437bc8dc5f \
//	    -journal <file>                       # dry run: prints the plan ID
//	    ... -execute -authorize <plan ID>     # the one authorized write
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/nitrado"
	"github.com/yourname/dayz-killfeed/internal/shop/canary"
	"github.com/yourname/dayz-killfeed/internal/shop/capability"
	"github.com/yourname/dayz-killfeed/internal/shop/missionwrite"
)

const (
	exitOK        = 0
	exitRefused   = 1 // nothing was attempted
	exitNotDone   = 2 // NOT_WRITTEN
	exitUncertain = 3 // UNCERTAIN: stop, inspect, resolve
)

func main() { os.Exit(run()) }

func run() int {
	op := flag.String("operation", "", "the one operation: gate-a-create-empty (gate-e-stage and gate-g-unstage are not active)")
	service := flag.String("service", "", "Nitrado service ID")
	org := flag.Int64("org", 0, "organization ID")
	inst := flag.Int64("installation", 0, "installation ID")
	gs := flag.Int64("game-server", 0, "game server ID")
	mission := flag.String("mission", "", "canonical mission path, e.g. dayzps_missions/dayzOffline.chernarusplus")
	dest := flag.String("path", "", "mission-relative destination")
	expectCurrent := flag.String("expect-current", "", "expected destination state: absent, or its SHA-256")
	expectConfig := flag.String("expect-config-sha256", "", "expected cfggameplay.json SHA-256")
	expectSpawners := flag.String("expect-spawners", "", `expected objectSpawnersArr as JSON, e.g. '["custom/The_Lost_City.json"]'`)
	expectPayload := flag.String("expect-payload-sha256", "", "expected payload SHA-256")
	journalPath := flag.String("journal", "", "local journal file (single-use authorizations, outcomes)")
	execute := flag.Bool("execute", false, "perform the write (requires -authorize)")
	authorize := flag.String("authorize", "", "the plan ID the owner approved")
	resolve := flag.String("resolve", "", "record the owner's resolution of an UNCERTAIN/interrupted plan ID (journal only; no server call)")
	note := flag.String("note", "", "resolution note (with -resolve)")
	timeout := flag.Duration("timeout", 120*time.Second, "overall timeout")
	flag.Parse()

	j, err := missionwrite.OpenJournal(*journalPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "refused:", err)
		return exitRefused
	}
	if *resolve != "" {
		if err := j.Resolve(*resolve, *service, *dest, *note); err != nil {
			fmt.Fprintln(os.Stderr, "refused:", err)
			return exitRefused
		}
		fmt.Println("resolution recorded for", *resolve)
		return exitOK
	}

	var spawners []string
	if err := json.Unmarshal([]byte(*expectSpawners), &spawners); err != nil || spawners == nil {
		fmt.Fprintln(os.Stderr, "refused: -expect-spawners must be a JSON array")
		return exitRefused
	}
	current := strings.TrimSpace(*expectCurrent)
	if strings.EqualFold(current, "absent") {
		current = missionwrite.Absent
	}
	req := missionwrite.Request{
		Operation:           missionwrite.Operation(*op),
		Binding:             capability.Binding{OrganizationID: *org, InstallationID: *inst, GameServerID: *gs, NitradoServiceID: *service},
		Mission:             *mission,
		Path:                *dest,
		ExpectCurrent:       current,
		ExpectConfigSHA256:  strings.ToLower(*expectConfig),
		ExpectSpawners:      spawners,
		Payload:             payloadFor(missionwrite.Operation(*op)),
		ExpectPayloadSHA256: strings.ToLower(*expectPayload),
	}
	if err := req.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "refused:", err)
		return exitRefused
	}
	if *execute && strings.TrimSpace(*authorize) == "" {
		fmt.Fprintln(os.Stderr, "refused:", missionwrite.ErrAuthorizationMissing)
		return exitRefused
	}
	token := os.Getenv("NITRADO_TOKEN")
	if token == "" {
		fmt.Fprintln(os.Stderr, "refused: NITRADO_TOKEN is not set")
		return exitRefused
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	client := nitrado.NewClient(nitrado.DefaultBaseURL, token, &http.Client{Timeout: 45 * time.Second})

	if !*execute {
		p, err := missionwrite.Prepare(ctx, client, req)
		printInspection(p.Inspection)
		if err != nil {
			fmt.Println("\nREFUSED:", err)
			return exitRefused
		}
		fmt.Println("\nplan (NOT EXECUTED):")
		for i, s := range p.Steps {
			fmt.Printf("  %d. %s\n", i+1, s)
		}
		fmt.Println("\nplan ID:", p.ID)
		fmt.Println("To execute exactly this plan once, re-run the same command with: -execute -authorize", p.ID)
		return exitOK
	}

	o, err := missionwrite.Execute(ctx, client, req, *authorize, j)
	if err != nil && o.Status == missionwrite.StatusNotWritten && o.Transfer == "not attempted" && len(o.Checks) == 0 {
		fmt.Println("REFUSED (nothing attempted):", err)
		return exitRefused
	}
	fmt.Println("plan ID:   ", o.PlanID)
	fmt.Println("status:    ", o.Status)
	fmt.Println("before:    ", o.Before)
	fmt.Printf("after:      %s (%d bytes)\n", o.After, o.AfterBytes)
	fmt.Println("directory: ", map[bool]string{true: "created", false: "not created"}[o.DirectoryMade])
	fmt.Println("transfer:  ", o.Transfer)
	for _, c := range o.Checks {
		fmt.Printf("  %-10s %-28s %s\n", c.Result, c.Name, c.Detail)
	}
	if err != nil {
		fmt.Println("ERROR:", err)
	}
	switch o.Status {
	case missionwrite.StatusWrittenVerified:
		return exitOK
	case missionwrite.StatusUncertain:
		fmt.Println("UNCERTAIN: stop. Do not retry. Inspect the destination read-only and record the owner's resolution with -resolve.")
		return exitUncertain
	}
	return exitNotDone
}

// payloadFor returns the operation's fixed payload: the tool never reads a payload from a file.
func payloadFor(op missionwrite.Operation) []byte {
	if op == missionwrite.OpCreateEmptyChampionFile {
		b, _ := canary.EmptyArtifact()
		return b
	}
	return nil
}

func printInspection(in missionwrite.Inspection) {
	if in.MissionPath == "" {
		return
	}
	fmt.Println("fresh read-only inspection:")
	fmt.Println("  mission:           ", in.MissionPath)
	fmt.Printf("  cfggameplay.json:   %d bytes, SHA-256 %s\n", in.ConfigBytes, in.ConfigSHA256)
	sp, _ := json.Marshal(in.Spawners)
	fmt.Println("  objectSpawnersArr: ", string(sp))
	fmt.Println("  champion/ exists:  ", in.ParentExists)
	if in.Current == missionwrite.Absent {
		fmt.Println("  destination:        absent")
	} else {
		fmt.Printf("  destination:        present, %d bytes, SHA-256 %s\n", in.CurrentBytes, in.Current)
	}
	fmt.Println("  gameserver status: ", in.GameserverStatus)
	fmt.Println("  current boot:      ", orUnverified(in.BootFile))
}

func orUnverified(s string) string {
	if s == "" {
		return "UNVERIFIED"
	}
	return s
}
