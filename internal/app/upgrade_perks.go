package app

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 24: (a) renewal reminders - three days before a donation subscription renews or ends, or
// a supporter tier runs out, the player gets a direct message (the perkReminderDms switch of any
// of the guild's servers); (b) gift notes - a player gifting a perk can add a short note, and the
// recipient gets it by direct message with the gift (always on).

const (
	perkRemindersEvery = 30 * time.Minute
	giftMessageMax     = 200
)

func buildExpiryReminder(e repository.Expiring) (title, desc string) {
	when := fmt.Sprintf("<t:%d:R>", e.ExpiresAt.Unix())
	switch {
	case e.Kind == "PERK" && e.Renews:
		return "⏰ Your " + e.Name + " renews soon", fmt.Sprintf("It renews %s for %d points. Keep enough in your wallet, or turn off renewal on the Donate tab.", when, e.PricePoints)
	case e.Kind == "PERK":
		return "⏰ Your " + e.Name + " ends soon", fmt.Sprintf("It ends %s. You can buy it again on the Donate tab.", when)
	default:
		return "⏰ Your " + e.Name + " tier ends soon", fmt.Sprintf("Your supporter tier ends %s. Thank you for supporting the server!", when)
	}
}

func (a *App) runPerkReminders(ctx context.Context, guildID int64, servers []repository.UpgradeServer, now time.Time) {
	on := false
	for _, s := range servers {
		on = on || s.Settings.PerkReminderDMs
	}
	if !on || !a.upgradeRuns.due(fmt.Sprintf("perks:%d", guildID), perkRemindersEvery, now) {
		return
	}
	due, err := a.Upgrades.ExpiringSoon(ctx, guildID, now, now.Add(72*time.Hour))
	if err != nil {
		return
	}
	for _, e := range due {
		title, desc := buildExpiryReminder(e)
		a.notifyOnce(ctx, "EXPIRY_"+e.Kind, guildID, 0, e.PlayerID, fmt.Sprintf("%d:%d", e.ID, e.ExpiresAt.Unix()), now,
			dm(upgradeEmbed("CHAMPIONS® SUPPORTERS", title, desc, 0xE7B94A)))
	}
}

// cleanGiftMessage trims a gift note to one safe paragraph (no mentions, no control characters).
func cleanGiftMessage(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
		case r == '@':
			b.WriteString("@\u200b")
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if r := []rune(out); len(r) > giftMessageMax {
		out = string(r[:giftMessageMax])
	}
	return out
}

// sendGiftNotice tells the recipient of a gifted perk who sent it, with their note.
func (a *App) sendGiftNotice(ctx context.Context, p repository.PerkPurchase, note string) {
	if a.Upgrades == nil || !p.Gift {
		return
	}
	if note != "" {
		if err := a.Upgrades.SaveGiftMessage(ctx, p.ID, note); err != nil {
			return
		}
	}
	desc := fmt.Sprintf("**%s** gifted you **%s**. Enjoy!", orUnknown(p.BuyerName), p.OfferName)
	if note != "" {
		desc += "\n\n> " + note
	}
	if claimed, err := a.Upgrades.ClaimNotice(ctx, "PERK_GIFT", p.ServerID, p.RecipientPlayerID, fmt.Sprint(p.ID), time.Now().UTC()); err != nil || !claimed {
		return
	}
	_ = a.sendPlayerDM(ctx, p.GuildID, p.RecipientPlayerID, dm(upgradeEmbed("CHAMPIONS® SUPPORTERS", "🎁 You got a gift", desc, 0xE7B94A)))
}
