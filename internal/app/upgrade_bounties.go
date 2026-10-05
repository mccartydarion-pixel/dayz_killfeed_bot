package app

import (
	"context"
	"fmt"
	"time"

	"github.com/yourname/dayz-killfeed/internal/bounties"
	"github.com/yourname/dayz-killfeed/internal/presentation"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 14, bounty messages: whoever placed a bounty hears by direct message when it is claimed
// (and by whom) or runs out unclaimed. It follows the bountyDms switch of the bounty's server, or
// of any server of the guild for a guild-wide bounty. Bounty cards never name who placed a
// bounty, so placing one is already anonymous.

func (a *App) bountyPlacerDMsOn(ctx context.Context, b repository.Bounty) bool {
	if a.Upgrades == nil {
		return false
	}
	if b.ServerID > 0 {
		s, err := a.Upgrades.Settings(ctx, b.ServerID)
		return err == nil && s.BountyDMs
	}
	servers, err := a.Upgrades.GuildServers(ctx, b.GuildID)
	if err != nil {
		return false
	}
	for _, s := range servers {
		if s.Settings.BountyDMs {
			return true
		}
	}
	return false
}

func buildBountyPlacerDM(target string, points int64, outcome, hunter string) (title, desc string) {
	target = orUnknown(target)
	if outcome == bounties.PlacerClaimed {
		return "🎯 Your bounty was claimed", fmt.Sprintf("**%s** took down **%s** and collected your **%s** bounty.", orUnknown(hunter), target, presentation.Plural(points, "point", "point"))
	}
	return "⌛ Your bounty ran out", fmt.Sprintf("Nobody claimed your **%s** bounty on **%s** before it expired.", presentation.Plural(points, "point", "point"), target)
}

// notifyBountyPlacer is the bounty service's placer hook. It runs after the claim or expiry
// committed and never blocks it for long.
func (a *App) notifyBountyPlacer(b repository.Bounty, outcome, hunter string) {
	if b.CreatedByDiscordUserID == "" || !isDiscordID(b.CreatedByDiscordUserID) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if !a.bountyPlacerDMsOn(ctx, b) {
			return
		}
		target := ""
		if a.Players != nil {
			if names, err := a.Players.DisplayNamesByID(ctx, b.GuildID, []int64{b.TargetPlayerID}); err == nil {
				target = names[b.TargetPlayerID]
			}
		}
		title, desc := buildBountyPlacerDM(target, b.RewardPoints, outcome, hunter)
		if claimed, err := a.Upgrades.ClaimNotice(ctx, "BOUNTY_"+outcome, b.ServerID, 0, fmt.Sprint(b.ID), time.Now().UTC()); err != nil || !claimed {
			return
		}
		_ = a.sendUserDM(b.CreatedByDiscordUserID, dm(upgradeEmbed("Bounties", title, desc, presentation.Crimson)))
	}()
}

// isDiscordID reports whether s looks like a Discord user id (a system source such as an automatic
// bounty is not one).
func isDiscordID(s string) bool {
	if len(s) < 15 || len(s) > 21 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
