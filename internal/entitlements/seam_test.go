package entitlements

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

// The seam for platform-owner access is the Plan type: Has, Resolve, RouteAllowed and FactionLimit
// take a Plan, and a Plan outside this package can only come from ForOrganization. The compiler
// enforces that part (a plan string no longer fits). What it cannot catch is a caller that builds
// a Plan without saying whose it is - ForOrganization(0, plan) - or a zero Plan{}. This walks the
// whole module and fails on either, so a new gate cannot quietly skip the platform-owner rule.
func TestEveryPlanQuestionNamesItsOrganization(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	checked := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == "entitlements" {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		alias := ""
		for _, imp := range file.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); strings.HasSuffix(p, "/internal/entitlements") {
				alias = "entitlements"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
			}
		}
		if alias == "" {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != alias || sel.Sel.Name != "ForOrganization" {
					return true
				}
				checked++
				if len(v.Args) != 2 {
					t.Errorf("%s: ForOrganization takes (organizationID, planKey)", fset.Position(v.Pos()))
					return true
				}
				if lit, ok := v.Args[0].(*ast.BasicLit); ok {
					t.Errorf("%s: ForOrganization is called with the literal organization id %s; pass the real organization so platform-owner access applies", fset.Position(v.Pos()), lit.Value)
				}
			case *ast.CompositeLit:
				if sel, ok := v.Type.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == alias && sel.Sel.Name == "Plan" {
						t.Errorf("%s: entitlements.Plan{} built by hand; use entitlements.ForOrganization", fset.Position(v.Pos()))
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
	if checked < 5 {
		t.Fatalf("found only %d ForOrganization call sites; the walk is not seeing the module", checked)
	}
}
