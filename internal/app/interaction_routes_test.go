package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/discord"
)

// Every slash command the bot registers at Discord must have a route on the
// interaction router, registered with an explicit acknowledgement mode, and
// every subcommand that is exempt from deferral must really exist. A command
// added without a route would time out with "The application did not respond";
// this test fails first.
func TestEveryRegisteredCommandHasAnAcknowledgedRoute(t *testing.T) {
	batch := discord.NewCommandBatch("app")
	registrars := map[string]func() error{
		"RegisterAdminCommands":      func() error { return discord.RegisterAdminCommands(batch, "g") },
		"RegisterAnalyticsCommands":  func() error { return discord.RegisterAnalyticsCommands(batch, "g") },
		"RegisterBaseCommands":       func() error { return discord.RegisterBaseCommands(batch, "g") },
		"RegisterBountyCommands":     func() error { return discord.RegisterBountyCommands(batch, "g") },
		"RegisterCardCommand":        func() error { return discord.RegisterCardCommand(batch, "g") },
		"RegisterEconomyCommands":    func() error { return discord.RegisterEconomyCommands(batch, "g") },
		"RegisterEventCommands":      func() error { return discord.RegisterEventCommands(batch, "g") },
		"RegisterFeaturesCommand":    func() error { return discord.RegisterFeaturesCommand(batch, "g") },
		"RegisterLifeCommands":       func() error { return discord.RegisterLifeCommands(batch, "g") },
		"RegisterLinkCommands":       func() error { return discord.RegisterLinkCommands(batch, "g") },
		"RegisterPointsCommands":     func() error { return discord.RegisterPointsCommands(batch, "g") },
		"RegisterSeasonCommands":     func() error { return discord.RegisterSeasonCommands(batch, "g") },
		"RegisterServerCommands":     func() error { return discord.RegisterServerCommands(batch, "g") },
		"RegisterSetupCommand":       func() error { return discord.RegisterSetupCommand(batch, "g") },
		"RegisterStatsCommands":      func() error { return discord.RegisterStatsCommands(batch, "g") },
		"RegisterTournamentCommands": func() error { return discord.RegisterTournamentCommands(batch, "g") },
		"RegisterWarCommands":        func() error { return discord.RegisterWarCommands(batch, "g") },
		"RegisterWelcomeCommands":    func() error { return discord.RegisterWelcomeCommands(batch, "g") },
	}
	for name, register := range registrars {
		if err := register(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// The command definitions: name -> subcommand paths, and whether any
	// option autocompletes.
	subcommands := map[string]map[string]bool{}
	autocompletes := map[string]bool{}
	var walk func(cmd string, prefix string, options []*discordgo.ApplicationCommandOption)
	walk = func(cmd, prefix string, options []*discordgo.ApplicationCommandOption) {
		for _, opt := range options {
			switch opt.Type {
			case discordgo.ApplicationCommandOptionSubCommandGroup:
				walk(cmd, strings.TrimSpace(prefix+" "+opt.Name), opt.Options)
			case discordgo.ApplicationCommandOptionSubCommand:
				subcommands[cmd][strings.TrimSpace(prefix+" "+opt.Name)] = true
				walk(cmd, "", opt.Options)
			default:
				if opt.Autocomplete {
					autocompletes[cmd] = true
				}
			}
		}
	}
	for _, cmd := range batch.Commands() {
		subcommands[cmd.Name] = map[string]bool{}
		walk(cmd.Name, "", cmd.Options)
	}
	if len(subcommands) < 20 {
		t.Fatalf("only %d commands collected", len(subcommands))
	}

	// The routes, read from this package's source.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	ackName := regexp.MustCompile(`^Ack(Private|Public|Update|Self)$`)
	routedCommands := map[string]bool{}
	routedAutocomplete := map[string]bool{}
	registerCalls := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "discord" && strings.HasPrefix(sel.Sel.Name, "Register") {
				registerCalls[sel.Sel.Name] = true
			}
			switch sel.Sel.Name {
			case "Command", "Component", "ComponentPrefix", "Modal", "ModalPrefix":
				if len(call.Args) < 3 {
					return true
				}
				ack, ok := call.Args[1].(*ast.SelectorExpr)
				if !ok || !ackName.MatchString(ack.Sel.Name) {
					t.Errorf("%s: route is registered without a named discord.Ack* mode", fset.Position(call.Pos()))
				}
				if sel.Sel.Name != "Command" {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok {
					t.Errorf("%s: command route name is not a string literal", fset.Position(call.Pos()))
					return true
				}
				cmd, _ := strconv.Unquote(lit.Value)
				routedCommands[cmd] = true
				// SubAck overrides must name a real subcommand.
				for _, arg := range call.Args[3:] {
					comp, ok := arg.(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, elt := range comp.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Path" {
							if pathLit, ok := kv.Value.(*ast.BasicLit); ok {
								path, _ := strconv.Unquote(pathLit.Value)
								if !subcommands[cmd][path] {
									t.Errorf("%s: /%s has no subcommand %q", fset.Position(call.Pos()), cmd, path)
								}
							}
						}
					}
				}
			case "Autocomplete":
				if len(call.Args) == 2 {
					if lit, ok := call.Args[0].(*ast.BasicLit); ok {
						cmd, _ := strconv.Unquote(lit.Value)
						routedAutocomplete[cmd] = true
					}
				}
			}
			return true
		})
	}

	for name := range registerCalls {
		if _, ok := registrars[name]; !ok {
			t.Errorf("discord.%s is called by the app but missing from this test's list: add it so its commands are checked", name)
		}
	}
	var missing, stale []string
	for cmd := range subcommands {
		if !routedCommands[cmd] {
			missing = append(missing, cmd)
		}
		if autocompletes[cmd] && !routedAutocomplete[cmd] {
			t.Errorf("/%s has an autocomplete option but no Autocomplete route", cmd)
		}
	}
	for cmd := range routedCommands {
		if _, ok := subcommands[cmd]; !ok {
			stale = append(stale, cmd)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("slash commands registered at Discord without a route (they would time out): %v", missing)
	}
	if len(stale) > 0 {
		t.Errorf("routes for commands that are not registered at Discord: %v", stale)
	}
}
