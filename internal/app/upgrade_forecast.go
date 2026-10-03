package app

import (
	"context"
	"sync"
	"time"
)

// Upgrade 9b, hot zone forecast: the two hours of the day when the server usually has the most
// PvP, from the last four weeks of kills, in UTC (the website shows it in the viewer's own time).
// It is cached for an hour per server.

const (
	forecastDays      = 28
	forecastWidth     = 2
	forecastMinKills  = 20
	forecastCacheTime = time.Hour
)

type hotZoneForecast struct {
	FromHour int `json:"fromHourUtc"`
	ToHour   int `json:"toHourUtc"`
	Kills    int `json:"kills"`
	Days     int `json:"days"`
}

// busiestHours is the start of the `width` consecutive hours (wrapping midnight) with the most
// kills, and their total.
func busiestHours(byHour [24]int, width int) (start, total int) {
	best := -1
	for h := 0; h < 24; h++ {
		sum := 0
		for i := 0; i < width; i++ {
			sum += byHour[(h+i)%24]
		}
		if sum > best {
			best, start = sum, h
		}
	}
	return start, best
}

type forecastCache struct {
	mu   sync.Mutex
	at   map[int64]time.Time
	vals map[int64]*hotZoneForecast
}

func (a *App) hotZoneForecastFor(ctx context.Context, guildID, serverID int64, now time.Time) *hotZoneForecast {
	if a.Retention == nil {
		return nil
	}
	c := &a.forecasts
	c.mu.Lock()
	if c.at == nil {
		c.at, c.vals = map[int64]time.Time{}, map[int64]*hotZoneForecast{}
	}
	if at, ok := c.at[serverID]; ok && now.Sub(at) < forecastCacheTime {
		v := c.vals[serverID]
		c.mu.Unlock()
		return v
	}
	c.mu.Unlock()
	rows, err := a.Retention.KillsByHour(ctx, guildID, serverID, now, forecastDays, "UTC")
	if err != nil {
		return nil
	}
	var byHour [24]int
	all := 0
	for _, r := range rows {
		if r.Hour >= 0 && r.Hour < 24 {
			byHour[r.Hour] += r.Kills
			all += r.Kills
		}
	}
	var out *hotZoneForecast
	if all >= forecastMinKills {
		start, total := busiestHours(byHour, forecastWidth)
		out = &hotZoneForecast{FromHour: start, ToHour: (start + forecastWidth) % 24, Kills: total, Days: forecastDays}
	}
	c.mu.Lock()
	c.at[serverID], c.vals[serverID] = now, out
	c.mu.Unlock()
	return out
}
