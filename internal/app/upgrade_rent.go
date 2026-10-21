package app

import (
	"context"
	"fmt"
	"time"

	"github.com/yourname/dayz-killfeed/internal/discord"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 19, rent reminders: on top of the built-in reminder (to the owner, a day before), the
// owner and their faction mates hear three days ahead, and the faction mates also get the
// one-day reminder - any of them can pay. Each person hears about each due date once.

const rentRemindersEvery = 10 * time.Minute

func (a *App) runRentReminders(ctx context.Context, guildID int64, s repository.UpgradeServer, now time.Time) {
	if !s.Settings.RentReminders || !a.upgradeRuns.due(fmt.Sprintf("rent:%d", s.ServerID), rentRemindersEvery, now) {
		return
	}
	serverName := a.serverName(s.ServerID)
	// Three days ahead: owner and faction.
	early, err := a.Upgrades.RentDueBetween(ctx, s.ServerID, now.Add(24*time.Hour), now.Add(72*time.Hour))
	if err == nil {
		for _, d := range early {
			msg := discord.BaseRentNoticeMessage(true, d.BaseName, serverName, d.DueAt, d.PricePoints, d.PeriodDays, a.securityStoreURL())
			ref := fmt.Sprintf("%d:%d:3d", d.BaseID, d.DueAt.Unix())
			for _, p := range append([]int64{d.OwnerID}, d.FactionMates...) {
				a.notifyOnce(ctx, "RENT_REMINDER", guildID, s.ServerID, p, ref, now, msg)
			}
		}
	}
	// The last day: faction mates (the owner already gets the built-in reminder).
	soon, err := a.Upgrades.RentDueBetween(ctx, s.ServerID, now, now.Add(24*time.Hour))
	if err == nil {
		for _, d := range soon {
			msg := discord.BaseRentNoticeMessage(true, d.BaseName, serverName, d.DueAt, d.PricePoints, d.PeriodDays, a.securityStoreURL())
			ref := fmt.Sprintf("%d:%d:1d", d.BaseID, d.DueAt.Unix())
			for _, p := range d.FactionMates {
				a.notifyOnce(ctx, "RENT_REMINDER", guildID, s.ServerID, p, ref, now, msg)
			}
		}
	}
}
