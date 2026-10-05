package missionwrite

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The write capability is isolated: only this package calls the Nitrado write primitives, and only
// tests relax the https requirement. Exactly two things may import this package:
//
//   - cmd/shop-mission-write, the operator's guarded tool (every gate);
//   - internal/shop/deliveryworker, the automatic delivery worker approved in
//     docs/SHOP_DELIVERY_WORKER_DESIGN.md. It may use ONLY the two artifact primitives
//     (InspectArtifact, WriteArtifact) and the types and values they return: never Prepare, Execute,
//     an Operation or the configuration gates. WriteArtifact can write one fixed file,
//     custom/champion_shop_delivery.json, and nothing else.
//
// And the worker itself is reachable from exactly one file of the bot, internal/app/shop_delivery_worker.go,
// which starts it only behind config.ShopAutoDelivery. Nothing else (startup, Live Sync, the Shop
// service, an API handler) may import either package.
//
// One more package may call the Nitrado write primitives, and only the two it needs
// (RequestUploadToken and PostUpload, never Mkdir): internal/maprotation/mapswitch, the map
// rotation switch approved in docs/MAP_ROTATION.md. It writes two fixed mission files
// (cfggameplay.json and cfgplayerspawnpoints.xml) and nothing else, and it is reachable from
// exactly one file of the bot, internal/app/map_rotation_worker.go, which runs it only behind the
// map_rotation feature flag, the plan and the owner's own switch. It does not import missionwrite.
//
// And one package may call the Nitrado delete call (DeleteFile), which nothing else in Champion may:
// internal/maprotation/charwipe, "fresh characters on every map switch" in docs/MAP_ROTATION.md. It
// deletes one fixed file, <mission folder>/storage_1/players.db, and refuses every other path. It
// may not call the write primitives, it does not import missionwrite or the map switch, and it too
// is reachable only from internal/app/map_rotation_worker.go.
func TestWriteCapabilityIsIsolated(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const self = "internal/shop/missionwrite"
	const worker = "internal/shop/deliveryworker"
	const workerEntry = "internal/app/shop_delivery_worker.go"
	const mapSwitch = "internal/maprotation/mapswitch"
	const mapSwitchEntry = "internal/app/map_rotation_worker.go"
	mapSwitchWrites := map[string]bool{"RequestUploadToken": true, "PostUpload": true}
	seenMapSwitchEntry := false
	const charWipe = "internal/maprotation/charwipe"
	deletes := map[string]bool{"DeleteFile": true}
	seenCharWipeEntry, seenDelete := false, false
	// What the worker may name from this package: the artifact primitives and what they return.
	workerMay := map[string]bool{"InspectArtifact": true, "WriteArtifact": true, "ArtifactState": true, "ArtifactWrite": true, "Remote": true,
		"SHA256": true, "StatusWrittenVerified": true, "StatusNotWritten": true, "StatusUncertain": true,
		"ErrArtifactMissing": true, "ErrArtifactNotReferenced": true, "ErrBinding": true}
	seenWorker, seenEntry := false, false
	writes := map[string]bool{"RequestUploadToken": true, "PostUpload": true, "Mkdir": true}
	seenCmd := false
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "web") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		dir := filepath.ToSlash(filepath.Dir(rel))
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			return err
		}
		for _, im := range f.Imports {
			ip, _ := strconv.Unquote(im.Path.Value)
			if strings.HasSuffix(ip, "/"+self) {
				switch dir {
				case "cmd/shop-mission-write":
					seenCmd = true
				case worker:
					seenWorker = true
					if im.Name != nil {
						t.Errorf("%s imports missionwrite under another name, which hides what it uses", rel)
					}
				default:
					t.Errorf("%s imports missionwrite", rel)
				}
			}
			if strings.HasSuffix(ip, "/"+worker) {
				if rel != workerEntry {
					t.Errorf("%s imports the delivery worker; only %s may", rel, workerEntry)
				}
				seenEntry = true
			}
			if strings.HasSuffix(ip, "/"+mapSwitch) {
				if rel != mapSwitchEntry {
					t.Errorf("%s imports the map switch; only %s may", rel, mapSwitchEntry)
				}
				seenMapSwitchEntry = true
			}
			if strings.HasSuffix(ip, "/"+charWipe) {
				if rel != mapSwitchEntry {
					t.Errorf("%s imports the character wipe; only %s may", rel, mapSwitchEntry)
				}
				seenCharWipeEntry = true
			}
			if dir == charWipe && (strings.HasSuffix(ip, "/"+self) || strings.HasSuffix(ip, "/"+mapSwitch)) {
				t.Errorf("%s imports %s; the character wipe writes nothing", rel, ip)
			}
			if dir == mapSwitch && strings.HasSuffix(ip, "/"+self) {
				t.Errorf("%s imports missionwrite; the map switch has its own two-file write", rel)
			}
		}
		if dir == worker {
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "missionwrite" && !workerMay[sel.Sel.Name] {
						t.Errorf("%s uses missionwrite.%s; the worker may use only the artifact primitives", rel, sel.Sel.Name)
					}
				}
				return true
			})
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && writes[sel.Sel.Name] && !isOS(sel) && dir != self && dir != "internal/nitrado" &&
					!(dir == mapSwitch && mapSwitchWrites[sel.Sel.Name]) {
					t.Errorf("%s calls %s", rel, sel.Sel.Name)
				}
				// The delete call: the Nitrado client defines it, the character wipe calls it, nobody else.
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && deletes[sel.Sel.Name] && !isOS(sel) {
					if dir != charWipe && dir != "internal/nitrado" {
						t.Errorf("%s calls %s; only %s may delete a file on a game server", rel, sel.Sel.Name, charWipe)
					}
					seenDelete = seenDelete || dir == charWipe
				}
			case *ast.AssignStmt:
				for _, l := range x.Lhs {
					if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "AllowInsecureUploadURL" {
						t.Errorf("%s relaxes the https requirement", rel)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seenCmd {
		t.Fatal("expected cmd/shop-mission-write to import this package")
	}
	if !seenMapSwitchEntry {
		t.Fatalf("expected %s to import the map switch", mapSwitchEntry)
	}
	if !seenCharWipeEntry || !seenDelete {
		t.Fatalf("expected %s to import the character wipe (%v) and the wipe to call DeleteFile (%v)", mapSwitchEntry, seenCharWipeEntry, seenDelete)
	}
	if !seenWorker || !seenEntry {
		t.Fatalf("expected the delivery worker to import this package (%v) and %s to import the worker (%v)", seenWorker, workerEntry, seenEntry)
	}
}

func isOS(sel *ast.SelectorExpr) bool {
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "os"
}
