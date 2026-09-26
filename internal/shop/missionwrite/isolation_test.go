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

// The write capability is isolated: only this package calls the Nitrado write primitives, only
// cmd/shop-mission-write imports this package (never the bot, its startup, Live Sync or Shop
// delivery), and only tests relax the https requirement.
func TestWriteCapabilityIsIsolated(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	const self = "internal/shop/missionwrite"
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
				if dir != "cmd/shop-mission-write" {
					t.Errorf("%s imports missionwrite", rel)
				}
				seenCmd = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && writes[sel.Sel.Name] && !isOS(sel) && dir != self && dir != "internal/nitrado" {
					t.Errorf("%s calls %s", rel, sel.Sel.Name)
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
}

func isOS(sel *ast.SelectorExpr) bool {
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "os"
}
