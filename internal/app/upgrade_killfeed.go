package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Upgrade 7, ranked tags on kill cards: the kill card shows the RP the kill earned, with an icon for
// double RP and each bonus ("+250 RP ⚡💀"). The switch is read through a one-minute cache, so the
// kill path does not query it for every kill.

var rankedBonusIcons = map[string]string{repository.BonusBounty: "💀", repository.BonusUnderdog: "🐺", repository.BonusRevenge: "🔁", repository.BonusDailyFirst: "☀️"}

// rankedTag is the card's badge for an award; "" when it earned nothing.
func rankedTag(a repository.RankedAward) string {
	if a.Outcome != "AWARDED" || a.Amount <= 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "+%d RP ", a.Amount)
	if a.Multiplier > 1 {
		b.WriteString("⚡")
	}
	for _, bonus := range a.Bonuses {
		b.WriteString(rankedBonusIcons[bonus.Kind])
	}
	return strings.TrimSpace(b.String())
}

type rankedTagCache struct {
	mu   sync.Mutex
	at   map[int64]time.Time
	vals map[int64]bool
}

func (a *App) killfeedRankedTagsOn(ctx context.Context, serverID int64) bool {
	if a.Upgrades == nil {
		return false
	}
	c := &a.rankedTagsCache
	c.mu.Lock()
	if c.at == nil {
		c.at, c.vals = map[int64]time.Time{}, map[int64]bool{}
	}
	if at, ok := c.at[serverID]; ok && time.Since(at) < time.Minute {
		v := c.vals[serverID]
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	s, err := a.Upgrades.Settings(ctx, serverID)
	on := err == nil && s.KillfeedRankedTags
	c.mu.Lock()
	c.at[serverID], c.vals[serverID] = time.Now(), on
	c.mu.Unlock()
	return on
}
