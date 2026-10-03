package app

import (
	"context"
	"fmt"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 23, shop order updates: the buyer hears by direct message as their order moves along -
// received, waiting for the next restart (automatic delivery), held up for staff, refunded. The
// "delivered" message with its buttons is the shop's own and is unchanged. Each step is sent once.

const shopStepsEvery = 2 * time.Minute

// orderSteps are the update messages an order is due, by step key.
func orderSteps(o repository.OrderStep) []string {
	steps := []string{"RECEIVED"}
	switch o.AttemptState {
	case "FILE_STAGED", "AWAITING_RESTART", "RESTART_OBSERVED", "VERIFICATION_REQUIRED":
		steps = append(steps, "AWAITING_RESTART")
	case "FAILED_REVIEW":
		steps = append(steps, "HELD")
	}
	if o.Status == "REFUNDED" {
		steps = append(steps, "REFUNDED")
	}
	return steps
}

func orderStepMessage(step string, o repository.OrderStep, serverName string) (title, desc string) {
	items := o.Items
	if items == "" {
		items = fmt.Sprintf("order #%d", o.PurchaseID)
	}
	switch step {
	case "RECEIVED":
		return "📦 Order received", fmt.Sprintf("Thanks! We got your order on %s: **%s** (%d points). We'll message you as it moves along.", serverWord(serverName), items, o.TotalPoints)
	case "AWAITING_RESTART":
		return "🚚 Your order is on its way", fmt.Sprintf("**%s** is ready and arrives in game at the next server restart.", items)
	case "HELD":
		return "⚠️ Your order needs a hand", fmt.Sprintf("Delivering **%s** hit a problem. Staff have been told and will sort it out.", items)
	default:
		return "↩️ Your order was refunded", fmt.Sprintf("**%s** was refunded: %d points are back in your wallet.", items, o.TotalPoints)
	}
}

func (a *App) runShopOrderUpdates(ctx context.Context, guildID int64, s repository.UpgradeServer, now time.Time) {
	if !s.Settings.ShopProgressDMs || !a.upgradeRuns.due(fmt.Sprintf("shop:%d", s.ServerID), shopStepsEvery, now) {
		return
	}
	// Only orders placed since the switch went on are followed, so turning it on sends nothing old.
	since := now.Add(-48 * time.Hour)
	if on := s.Settings.OnSince("shopProgressDms"); on.After(since) {
		since = on
	}
	orders, err := a.Upgrades.RecentOrders(ctx, s.ServerID, since)
	if err != nil {
		return
	}
	serverName := a.serverName(s.ServerID)
	for _, o := range orders {
		for _, step := range orderSteps(o) {
			title, desc := orderStepMessage(step, o, serverName)
			a.notifyOnce(ctx, "SHOP_STEP", guildID, s.ServerID, o.PlayerID, fmt.Sprintf("%d:%s", o.PurchaseID, step), now,
				dm(upgradeEmbed("CHAMPIONS® SHOP", title, desc, 0x2ECC71)))
		}
	}
}
