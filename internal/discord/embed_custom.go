package discord

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/economy"
	"github.com/yourname/dayz-killfeed/internal/embedrender"
	"github.com/yourname/dayz-killfeed/internal/killfeed"
	"github.com/yourname/dayz-killfeed/internal/presentation"
)

// Custom embed templates (Embed Designer Phase 4). Publishers keep building their
// existing Champion default embed exactly as before and pass it, with the event's
// variables, to an EmbedCustomizer (*embedrender.Renderer). The customizer returns
// either that same default or the custom rendering; a nil customizer (the rollout
// flag is off) makes every helper here a no-op. Presentation only: nothing below
// touches routing, aggregation, rate limits, persistence or ordering.

// EmbedCustomizer is the renderer surface publishers need.
type EmbedCustomizer interface {
	Customize(ctx context.Context, guildRowID, serverID int64, routeKey string, vars map[string]string, at time.Time, def *discordgo.MessageEmbed) *discordgo.MessageEmbed
}

// ServerNameFunc resolves a game server's display name for {{server_name}} ("" =
// unknown, which makes the variable absent).
type ServerNameFunc func(serverID int64) string

// customEmbed is the nil-safe call every publisher makes. The vars closure is only
// evaluated when a customizer is wired, so the default path pays nothing.
func customEmbed(c EmbedCustomizer, guildRowID, serverID int64, routeKey string, def *discordgo.MessageEmbed, vars func() map[string]string) *discordgo.MessageEmbed {
	if c == nil || def == nil || guildRowID <= 0 || serverID <= 0 {
		return def
	}
	return c.Customize(context.Background(), guildRowID, serverID, routeKey, vars(), time.Now(), def)
}

func serverNameOf(f ServerNameFunc, serverID int64) string {
	if f == nil || serverID <= 0 {
		return ""
	}
	return f(serverID)
}

// nameVar is a player-facing identity value: the raw name (the renderer sanitizes it),
// or "Unknown" when nothing usable remains - the same fallback the default cards use.
func nameVar(s string) string {
	if strings.TrimSpace(embedrender.SanitizeValue(s)) == "" {
		return "Unknown"
	}
	return s
}

func stampVar(at time.Time) string { return at.UTC().Format("2006-01-02 15:04:05 UTC") }

func setIf(m map[string]string, k, v string) {
	if strings.TrimSpace(v) != "" {
		m[k] = v
	}
}

// killfeedVars: only values the authoritative kill event carries.
func killfeedVars(ev *killfeed.Event, serverName string) map[string]string {
	m := map[string]string{"timestamp": stampVar(time.Now())}
	if ev.Killer != nil {
		m["killer"] = nameVar(ev.Killer.Name)
	} else {
		m["killer"] = "Unknown"
	}
	if ev.Victim != nil {
		m["victim"] = nameVar(ev.Victim.Name)
	} else {
		m["victim"] = "Unknown"
	}
	melee := isMelee(ev)
	setIf(m, "weapon", ev.Weapon)
	if ev.Weapon != "" || melee {
		setIf(m, "weapon_category", presentation.WeaponCategory(ev.Weapon, melee))
	}
	if ev.Distance != nil {
		m["distance"] = fmt.Sprintf("%.1fm", math.Round(*ev.Distance*10)/10)
	}
	setIf(m, "range", presentation.RangeClass(ev.Distance, melee))
	setIf(m, "ammo", ev.Ammo)
	// Hit data: only when the event itself carries it, or when the lethal hit was reliably
	// correlated with this kill (killfeed.FinalHit: the ADM line immediately before the kill, same
	// boot file, same players, second, weapon and distance) - never inferred or approximated.
	setIf(m, "hit_zone", ev.HitZone)
	if ev.Damage != nil {
		m["damage"] = formatDamage(*ev.Damage)
	}
	if fh := ev.FinalHit; fh != nil && ev.HitZone == "" && ev.Damage == nil {
		setIf(m, "hit_zone", fh.Zone)
		if fh.Damage != nil {
			m["damage"] = formatDamage(*fh.Damage)
		}
	}
	if isHeadshot(ev) {
		m["headshot"] = "HEADSHOT"
	}

	// Stats and streaks were attached to the event after durable persistence
	// (ProcessPersistedKill) - read here, never re-queried.
	if s := ev.KillerStats; s != nil {
		m["killer_kills"], m["killer_deaths"], m["killer_kd"] = presentation.FormatThousands(s.Kills), presentation.FormatThousands(s.Deaths), presentation.FormatKD(s.KD())
	}
	if s := ev.VictimStats; s != nil {
		m["victim_kills"], m["victim_deaths"], m["victim_kd"] = presentation.FormatThousands(s.Kills), presentation.FormatThousands(s.Deaths), presentation.FormatKD(s.KD())
	}
	if ev.KillerStreak != nil && *ev.KillerStreak > 0 {
		streak := strconv.Itoa(*ev.KillerStreak)
		m["streak"], m["killer_streak"] = streak, streak
	}
	if ev.StreakEnded && ev.EndedStreakCount != nil && *ev.EndedStreakCount > 0 {
		m["ended_streak"] = strconv.Itoa(*ev.EndedStreakCount)
	}
	if ev.KillingSpree {
		m["killing_spree"] = "KILLING SPREE"
	}
	if ev.StreakEnded {
		m["streak_ended"] = "STREAK ENDED"
	}
	if h := ev.Encounters; h != nil {
		m["h2h_killer_wins"], m["h2h_victim_wins"] = strconv.FormatInt(h.KillerWins, 10), strconv.FormatInt(h.VictimWins, 10)
		m["h2h_score"] = fmt.Sprintf("%d–%d", h.KillerWins, h.VictimWins)
	}

	// Story values come from the same classification the default card uses.
	p := BuildPresentation(ev)
	if p.Story != presentation.StoryStandard && strings.TrimSpace(p.Hero) != "" {
		m["special_kill"] = p.Hero
	}
	setIf(m, "kill_type", p.Title)
	if strings.TrimSpace(p.Title) != "" {
		m["story_title"] = strings.TrimSpace(p.Icon + " " + p.Title)
	}

	if ev.BountyClaimed && ev.BountyPoints > 0 {
		m["bounty_amount"] = formatAmount(ev.BountyPoints)
	}
	if ev.BountyTarget {
		m["bounty_target"] = "MOST WANTED"
	}
	if ev.BountyClaimed {
		m["bounty_claimed"] = "BOUNTY CLAIMED"
	}
	setIf(m, "season_name", ev.SeasonName)
	setIf(m, "war_badge", ev.WarBadge)
	var badges []string
	for _, b := range ev.ActiveEventBadges {
		if b = strings.TrimSpace(b); b != "" {
			badges = append(badges, b)
		}
	}
	setIf(m, "event_badges", strings.Join(badges, " • "))
	setIf(m, "server_name", serverName)
	return m
}

// formatDamage shows one decimal, or a whole number when the damage is whole.
func formatDamage(d float64) string {
	r := math.Round(d*10) / 10
	if r == math.Trunc(r) {
		return strconv.FormatFloat(r, 'f', 0, 64)
	}
	return strconv.FormatFloat(r, 'f', 1, 64)
}

// hitfeedVars: one aggregated encounter. Damage is only reported when EVERY hit
// carried one (a partial sum is never presented as the total); unknown values are absent.
func hitfeedVars(e *hitEncounter, serverName string) map[string]string {
	m := map[string]string{"timestamp": stampVar(time.Now())}
	a := nameVar(e.attacker)
	m["attacker"], m["killer"] = a, a // `killer` is the website's name for the attacker
	m["victim"] = nameVar(e.victim)
	setIf(m, "weapon", e.weapon)
	setIf(m, "ammo", strings.TrimPrefix(e.ammo, "Bullet_"))
	if e.distance != nil {
		m["distance"] = fmt.Sprintf("%.0fm", *e.distance)
	}
	setIf(m, "hit_zone", e.zone)
	if e.damageHits > 0 && e.damageHits == e.hits {
		m["damage"] = fmt.Sprintf("%.0f", e.damage)
	}
	if e.hits > 0 {
		m["hits"] = strconv.Itoa(e.hits)
	}
	setIf(m, "server_name", serverName)
	return m
}

// pveVars: only what the parser proved - the name and the classified cause.
func pveVars(n killfeed.PveDeathNotice, serverName string) map[string]string {
	m := map[string]string{"victim": nameVar(n.Name), "timestamp": stampVar(time.Now())}
	switch n.Cause {
	case killfeed.DeathCauseSuicide:
		m["cause"] = "suicide"
	case killfeed.DeathCauseInfected:
		m["cause"] = "infected"
	case killfeed.DeathCauseAnimal:
		m["cause"] = "animal"
	case killfeed.DeathCauseEnvironment:
		m["cause"] = "environment"
	}
	setIf(m, "server_name", serverName)
	return m
}

// connectionVars: one connect/disconnect state change.
func connectionVars(n killfeed.ConnectionNotice, serverName string) map[string]string {
	m := map[string]string{"player": nameVar(n.Name), "timestamp": stampVar(time.Now())}
	if n.Kind == killfeed.ConnectionConnected {
		m["event"], m["event_type"] = "joined", "connected"
	} else {
		m["event"], m["event_type"] = "left", "disconnected"
		setIf(m, "session", formatSession(n.Session))
	}
	setIf(m, "server_name", serverName)
	return m
}

// bountyVars: one lifecycle event; claim-only values are absent on the other kinds.
func bountyVars(e bounties.Event, serverName string) map[string]string {
	m := map[string]string{"status": strings.ToLower(string(e.Kind)), "amount": formatAmount(e.Amount)}
	t := nameVar(e.Target)
	m["target"], m["victim"] = t, t // `victim` is the website's name for the target
	if e.Kind == bounties.EventClaimed {
		h := nameVar(e.Hunter)
		m["hunter"], m["killer"] = h, h
		m["total"] = formatAmount(e.Amount)
		if e.Count > 0 {
			m["count"] = strconv.Itoa(e.Count)
		}
		setIf(m, "weapon", strings.TrimSpace(e.Weapon))
		if e.Distance != nil {
			m["distance"] = fmt.Sprintf("%.0fm", *e.Distance)
		}
	}
	setIf(m, "server_name", serverName)
	return m
}

// economyVars: one committed transaction. The balance is only known (and only shown
// by the default card) for rewards.
func economyVars(e economy.Event, serverName string) map[string]string {
	m := map[string]string{"player": nameVar(e.PlayerName), "amount": formatAmount(e.Amount), "transaction_type": economy.TypeLabel(e.Type)}
	if e.Type == economy.TypeBountyClaim || e.Type == economy.TypeSystemReward {
		m["balance"] = formatAmount(e.BalanceAfter)
	}
	setIf(m, "server_name", serverName)
	return m
}
