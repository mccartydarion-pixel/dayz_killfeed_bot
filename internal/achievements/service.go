package achievements

// Definition is a code-defined achievement. Unlocks are persisted separately.
type Definition struct {
	Key         string
	Name        string
	Description string
	Emoji       string
	Hidden      bool
}

type Unlock struct {
	Key   string
	Name  string
	Emoji string
}

var Definitions = []Definition{
	{"FIRST_KILL", "First Kill", "Score your first authoritative PvP kill.", "⚔️", false},
	{"FIRST_BLOOD", "First Blood", "Draw first blood in an ADM session.", "🩸", false},
	{"KILL_STREAK_5", "Killing Spree", "Reach a 5 kill streak.", "🔥", false},
	{"KILL_STREAK_10", "Dominating", "Reach a 10 kill streak.", "⚔️", false},
	{"KILL_STREAK_20", "Champion", "Reach a 20 kill streak.", "🏆", false},
	{"LONG_SHOT_100", "Sharpshooter", "Score a confirmed 100m+ kill.", "🎯", false},
	{"LONG_SHOT_200", "Marksman", "Score a confirmed 200m+ kill.", "🎯", false},
	{"LONG_SHOT_300", "Distance Elite", "Score a confirmed 300m+ kill.", "👑", false},
	{"KILLS_10", "Ten Kills", "Reach 10 lifetime kills.", "⚔️", false},
	{"KILLS_50", "Fifty Kills", "Reach 50 lifetime kills.", "🏅", false},
	{"KILLS_100", "Century", "Reach 100 lifetime kills.", "🏆", false},
}

func Find(key string) (Definition, bool) {
	for _, d := range Definitions {
		if d.Key == key {
			return d, true
		}
	}
	return Definition{}, false
}
