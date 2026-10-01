// Package staffactivity groups admin audit actions into the categories the Client Hub's staff
// activity log filters by. Patterns are SQL LIKE patterns, so the same table drives both the Go
// labelling and the database filter; the first matching category wins.
package staffactivity

import "strings"

type Category struct {
	Key, Label string
	Patterns   []string
}

// Categories in match order. "Viewed private data" comes first so CASE_..._VIEWED is a view.
var Categories = []Category{
	{"VIEWS", "Viewed private data", []string{"%_VIEWED"}},
	{"MODERATION", "Moderation", []string{"WARNING_%", "PLAYER_%", "BOUNTY_%", "CASE_%", "INTRUSION_%", "ZONE_BAN_%"}},
	{"ACCESS", "Staff & access", []string{"PERMISSION%", "WHITELIST_%", "BANLIST_%"}},
	{"SERVER", "Server control", []string{"SERVER_%", "MAINTENANCE_%", "FEED_%"}},
	{"ECONOMY", "Economy & rewards", []string{"ECONOMY_%", "SHOP_%", "POINTS_%", "REWARD_%", "VIP_%", "BASE_RENT_%", "SECURITY_GIFT_%"}},
	{"COMMUNITY", "Events, seasons & factions", []string{"EVENT_%", "SEASON_%", "FACTION_%"}},
	{"ZONES", "Zones & bases", []string{"ZONE_%", "PERIMETER_%", "BASE_%", "SECURITY_%"}},
}

const Other = "OTHER"

func like(pattern, s string) bool {
	switch {
	case strings.HasPrefix(pattern, "%"):
		return strings.HasSuffix(s, strings.TrimPrefix(pattern, "%"))
	case strings.HasSuffix(pattern, "%"):
		return strings.HasPrefix(s, strings.TrimSuffix(pattern, "%"))
	}
	return s == pattern
}

// Of returns the category key for an audit action.
func Of(action string) string {
	for _, c := range Categories {
		for _, p := range c.Patterns {
			if like(p, action) {
				return c.Key
			}
		}
	}
	return Other
}

// Find returns a category by key.
func Find(key string) (Category, bool) {
	for _, c := range Categories {
		if c.Key == key {
			return c, true
		}
	}
	return Category{}, false
}
