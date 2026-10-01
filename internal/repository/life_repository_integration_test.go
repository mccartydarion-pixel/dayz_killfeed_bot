//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Lives and retention collection (docs/LIVES.md, docs/RETENTION.md) against a real PostgreSQL.

type lifeWorld struct {
	*locationWorld
	lives    *LifeRepository
	activity *ActivityRepository
	deaths   *DeathRepository
	kills    *KillRepository
}

func newLifeWorld(t *testing.T) *lifeWorld {
	w := newLocationWorld(t)
	return &lifeWorld{locationWorld: w, lives: NewLifeRepository(w.db.Pool), activity: NewActivityRepository(w.db.Pool),
		deaths: NewDeathRepository(w.db.Pool), kills: NewKillRepository(w.db.Pool)}
}

// die persists a death row the way the persistence worker does, then closes the life.
func (w *lifeWorld) die(playerID int64, at time.Time, deathType string) *Life {
	w.t.Helper()
	fp := fmt.Sprintf("death-%d-%d", playerID, at.UnixNano())
	if err := w.deaths.InsertDeath(context.Background(), DeathRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: fp, PlayerID: playerID, DeathType: deathType, EventTime: &at}); err != nil {
		w.t.Fatal(err)
	}
	life, err := w.lives.RecordDeath(context.Background(), LifeEndInput{GuildID: w.guildID, ServerID: w.serverID, PlayerID: playerID, At: at, Fingerprint: fp})
	if err != nil {
		w.t.Fatal(err)
	}
	return life
}

// kill persists a PvP kill and closes the victim's life, the way the persistence worker does.
func (w *lifeWorld) kill(killerID, victimID int64, at time.Time, distance float64, headshot bool) *Life {
	w.t.Helper()
	fp := fmt.Sprintf("kill-%d-%d-%d", killerID, victimID, at.UnixNano())
	id, err := w.kills.InsertKillReturning(context.Background(), KillRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s",
		Fingerprint: fp, KillerPlayerID: killerID, VictimPlayerID: victimID,
		WeaponRaw: "M4A1", WeaponDisplay: "M4A1", Distance: &distance, Headshot: headshot, EventTime: &at})
	if err != nil {
		w.t.Fatal(err)
	}
	life, err := w.lives.RecordDeath(context.Background(), LifeEndInput{GuildID: w.guildID, ServerID: w.serverID, PlayerID: victimID, At: at, Fingerprint: fp,
		Kill: &LifeEndKill{KillID: id, KillerPlayerID: killerID, Weapon: "M4A1", Distance: &distance}})
	if err != nil {
		w.t.Fatal(err)
	}
	return life
}

func (w *lifeWorld) locate(playerID int64, at time.Time, x, z float64) {
	w.t.Helper()
	if _, err := w.db.Pool.Exec(context.Background(), `INSERT INTO player_location_events(guild_id,server_id,player_id,gamertag,x,z,event_type,observed_at) VALUES($1,$2,$3,'g',$4,$5,'OTHER_ADM',$6)`,
		w.guildID, w.serverID, playerID, x, z, at); err != nil {
		w.t.Fatal(err)
	}
}

// play connects at from and checkpoints every 30 seconds until to, then disconnects.
func (w *lifeWorld) play(playerID int64, from, to time.Time) {
	w.t.Helper()
	ctx := context.Background()
	if err := w.activity.Connect(ctx, w.guildID, w.serverID, playerID, from); err != nil {
		w.t.Fatal(err)
	}
	for at := from.Add(30 * time.Second); at.Before(to); at = at.Add(30 * time.Second) {
		if err := w.activity.CheckpointConnected(ctx, w.guildID, w.serverID, at); err != nil {
			w.t.Fatal(err)
		}
	}
	if err := w.activity.Disconnect(ctx, w.guildID, w.serverID, playerID, to); err != nil {
		w.t.Fatal(err)
	}
}

func TestLifeFirstLifeSpansAllPlaytimeAndNextLifeUsesMark(t *testing.T) {
	w := newLifeWorld(t)
	ctx := context.Background()
	hero, villain := w.seedPlayer("Hero"), w.seedPlayer("Villain")
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	w.play(hero, t0, t0.Add(10*time.Minute))
	w.kill(hero, villain, t0.Add(4*time.Minute), 120, true)
	w.kill(hero, villain, t0.Add(6*time.Minute), 310, false)
	w.locate(hero, t0.Add(1*time.Minute), 1000, 1000)
	w.locate(hero, t0.Add(2*time.Minute), 1300, 1400) // 500 m
	w.locate(hero, t0.Add(3*time.Minute), 1300, 1500) // 100 m

	// Second session: killed by Villain five minutes in.
	t1 := t0.Add(time.Hour)
	if err := w.activity.Connect(ctx, w.guildID, w.serverID, hero, t1); err != nil {
		t.Fatal(err)
	}
	for at := t1.Add(30 * time.Second); !at.After(t1.Add(5 * time.Minute)); at = at.Add(30 * time.Second) {
		if err := w.activity.CheckpointConnected(ctx, w.guildID, w.serverID, at); err != nil {
			t.Fatal(err)
		}
	}
	deathAt := t1.Add(5*time.Minute + 10*time.Second)
	first := w.kill(villain, hero, deathAt, 42, false)
	if first == nil {
		t.Fatal("first life not recorded")
	}
	// A "died" line trailing the kill line for the same death is not a second life.
	if dup := w.die(hero, deathAt.Add(time.Second), DeathTypeUnknown); dup != nil {
		t.Fatalf("trailing died line recorded a second life: %+v", dup)
	}
	if first.DistanceM == nil || *first.DistanceM != 42 || first.KillID == nil {
		t.Fatalf("first life kill = %+v", first)
	}
	if first.PlaytimeSeconds == nil || *first.PlaytimeSeconds != 15*60+10 {
		t.Fatalf("first life playtime = %v, want 910 (10 min + 5 min 10 s)", first.PlaytimeSeconds)
	}
	if !first.StartedAt.Equal(t0) {
		t.Fatalf("first life started %s, want first seen %s", first.StartedAt, t0)
	}
	if first.Kills != 2 || first.Headshots != 1 || first.LongestKillM == nil || *first.LongestKillM != 310 {
		t.Fatalf("first life kills=%d headshots=%d longest=%v", first.Kills, first.Headshots, first.LongestKillM)
	}
	if first.TrackedDistance == nil || *first.TrackedDistance != 600 || first.LocationSamples != 3 {
		t.Fatalf("tracked distance = %v over %d samples, want 600 over 3", first.TrackedDistance, first.LocationSamples)
	}
	if first.Cause != LifeCausePVP || first.KillerPlayerID == nil || *first.KillerPlayerID != villain || first.Weapon != "M4A1" {
		t.Fatalf("cause=%s killer=%v weapon=%q", first.Cause, first.KillerPlayerID, first.Weapon)
	}

	// Second life: three more minutes of play, no kills, a "died" line only (OTHER) - the kill that
	// ended the first life three minutes earlier must not be read as this death's cause.
	for at := deathAt.Add(30 * time.Second); !at.After(deathAt.Add(3 * time.Minute)); at = at.Add(30 * time.Second) {
		if err := w.activity.CheckpointConnected(ctx, w.guildID, w.serverID, at); err != nil {
			t.Fatal(err)
		}
	}
	second := w.die(hero, deathAt.Add(3*time.Minute), DeathTypeUnknown)
	if second == nil {
		t.Fatal("second life not recorded")
	}
	// The mark was taken at the first death, mid-checkpoint: the 10 seconds between the last
	// checkpoint and that death belong to the first life and must not be counted again.
	if second.PlaytimeSeconds == nil || *second.PlaytimeSeconds != 3*60 {
		t.Fatalf("second life playtime = %v, want 180", second.PlaytimeSeconds)
	}
	if !second.StartedAt.Equal(deathAt) || second.Kills != 0 || second.Cause != LifeCauseOther || second.TrackedDistance != nil {
		t.Fatalf("second life = %+v", second)
	}

	lives, err := w.lives.PlayerLives(ctx, w.guildID, w.serverID, hero, 10)
	if err != nil || len(lives) != 2 || lives[0].ID != second.ID || lives[1].KillerName != "Villain" || lives[1].PlayerName != "Hero" {
		t.Fatalf("PlayerLives = %+v err=%v", lives, err)
	}
	// Villain never connected, so their lives have no playtime and do not rank.
	top, err := w.lives.TopLives(ctx, w.guildID, w.serverID, LifeMetricPlaytime, nil, 5)
	if err != nil || len(top) != 2 || top[0].ID != first.ID {
		t.Fatalf("TopLives playtime = %+v err=%v", top, err)
	}
	top, err = w.lives.TopLives(ctx, w.guildID, w.serverID, LifeMetricKills, nil, 5)
	// Villain's own two lives (killed twice by Hero) have no kills; Hero's first has two.
	if err != nil || len(top) != 1 || top[0].Kills != 2 {
		t.Fatalf("TopLives kills = %+v err=%v", top, err)
	}
	since := deathAt.Add(time.Minute)
	top, err = w.lives.TopLives(ctx, w.guildID, w.serverID, LifeMetricPlaytime, &since, 5)
	if err != nil || len(top) != 1 || top[0].ID != second.ID {
		t.Fatalf("TopLives since = %+v err=%v", top, err)
	}
	if _, err := w.lives.TopLives(ctx, w.guildID, w.serverID, "BOGUS", nil, 5); err == nil {
		t.Fatal("unknown metric accepted")
	}
	sum, err := w.lives.Summary(ctx, w.guildID, w.serverID, hero)
	if err != nil || sum.Lives != 2 || sum.LongestPlaytime == nil || *sum.LongestPlaytime != 910 || sum.MostKills != 2 || sum.DeathsByPVP != 1 || sum.DeathsByOther != 1 || sum.TotalTrackedMeters != 600 {
		t.Fatalf("Summary = %+v err=%v", sum, err)
	}
	got, err := w.lives.Get(ctx, w.guildID, w.serverID, first.ID)
	if err != nil || got == nil || got.ID != first.ID {
		t.Fatalf("Get = %+v err=%v", got, err)
	}
	if other, err := w.lives.Get(ctx, w.guildID+999, w.serverID, first.ID); err != nil || other != nil {
		t.Fatalf("Get leaked across guilds: %+v err=%v", other, err)
	}
}

func TestLifeDuplicateAndReplayAreNotRecordedTwice(t *testing.T) {
	w := newLifeWorld(t)
	ctx := context.Background()
	p := w.seedPlayer("Solo")
	t0 := time.Date(2026, 5, 2, 9, 0, 0, 0, time.UTC)
	w.play(p, t0, t0.Add(2*time.Minute))
	in := LifeEndInput{GuildID: w.guildID, ServerID: w.serverID, PlayerID: p, At: t0.Add(3 * time.Minute), Fingerprint: "fp-a"}
	if life, err := w.lives.RecordDeath(ctx, in); err != nil || life == nil {
		t.Fatalf("first record: %+v %v", life, err)
	}
	if life, err := w.lives.RecordDeath(ctx, in); err != nil || life != nil {
		t.Fatalf("same fingerprint recorded twice: %+v %v", life, err)
	}
	in.Fingerprint, in.At = "fp-b", in.At.Add(2*time.Second)
	if life, err := w.lives.RecordDeath(ctx, in); err != nil || life != nil {
		t.Fatalf("death two seconds later recorded as a new life: %+v %v", life, err)
	}
	if life, err := w.lives.RecordDeath(ctx, LifeEndInput{}); err != nil || life != nil {
		t.Fatalf("empty input: %+v %v", life, err)
	}
}

func TestLifeBeganBeforeRecordingHasUnknownPlaytime(t *testing.T) {
	w := newLifeWorld(t)
	ctx := context.Background()
	p := w.seedPlayer("Veteran")
	t0 := time.Date(2026, 5, 3, 9, 0, 0, 0, time.UTC)
	w.play(p, t0, t0.Add(20*time.Minute))
	// A death that predates player_lives: a deaths row with no life row.
	old := t0.Add(5 * time.Minute)
	if err := w.deaths.InsertDeath(ctx, DeathRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: "old", PlayerID: p, DeathType: DeathTypeUnknown, EventTime: &old}); err != nil {
		t.Fatal(err)
	}
	cur, err := w.lives.CurrentLife(ctx, w.guildID, w.serverID, p, t0.Add(21*time.Minute))
	if err != nil || cur == nil || cur.PlaytimeSeconds != nil || !cur.StartedAt.Equal(old) {
		t.Fatalf("current life before recording = %+v err=%v", cur, err)
	}
	suicideAt := t0.Add(30 * time.Minute)
	emote := suicideAt.Add(-3 * time.Second)
	if err := w.deaths.InsertDeath(ctx, DeathRecord{GuildID: w.guildID, ServerID: w.serverID, SessionID: "s", Fingerprint: "emote", PlayerID: p, DeathType: DeathTypeSuicide, EventTime: &emote}); err != nil {
		t.Fatal(err)
	}
	life := w.die(p, suicideAt, DeathTypeUnknown)
	if life == nil || life.PlaytimeSeconds != nil || !life.StartedAt.Equal(old) || life.Cause != LifeCauseSuicide {
		t.Fatalf("life = %+v", life)
	}
	// The next life is fully measured.
	w.play(p, suicideAt.Add(time.Minute), suicideAt.Add(4*time.Minute))
	cur, err = w.lives.CurrentLife(ctx, w.guildID, w.serverID, p, suicideAt.Add(5*time.Minute))
	if err != nil || cur == nil || cur.PlaytimeSeconds == nil || *cur.PlaytimeSeconds != 180 || cur.Online {
		t.Fatalf("current life after recording = %+v err=%v", cur, err)
	}
}

func TestLongestAliveRanksMeasuredInProgressLives(t *testing.T) {
	w := newLifeWorld(t)
	ctx := context.Background()
	a, b, gone := w.seedPlayer("Alpha"), w.seedPlayer("Bravo"), w.seedPlayer("Gone")
	t0 := time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC)
	w.play(gone, t0.Add(-40*24*time.Hour), t0.Add(-40*24*time.Hour).Add(2*time.Hour))
	w.play(a, t0, t0.Add(30*time.Minute))
	w.play(b, t0, t0.Add(10*time.Minute))
	w.kill(a, b, t0.Add(10*time.Minute), 10, false)
	w.play(b, t0.Add(20*time.Minute), t0.Add(24*time.Minute)) // Bravo's new life: four minutes so far
	now := t0.Add(time.Hour)
	rows, err := w.lives.LongestAlive(ctx, w.guildID, w.serverID, now, now.Add(-30*24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].PlayerName != "Alpha" || *rows[0].PlaytimeSeconds != 1800 || rows[0].Kills != 1 || rows[1].PlayerName != "Bravo" || *rows[1].PlaytimeSeconds != 240 {
		t.Fatalf("LongestAlive = %+v", rows)
	}
	if cur, err := w.lives.CurrentLife(ctx, w.guildID, w.serverID, w.seedPlayer("Never"), now); err != nil || cur != nil {
		t.Fatalf("never-observed player has a current life: %+v %v", cur, err)
	}
}

func TestDailyRollupMirrorsObservedSecondsAndSessions(t *testing.T) {
	w := newLifeWorld(t)
	ctx := context.Background()
	p, q := w.seedPlayer("Daily"), w.seedPlayer("Other")
	day := time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)
	// 23:50 -> 00:10 crosses midnight; a second session later the next day.
	w.play(p, day.Add(23*time.Hour+50*time.Minute), day.Add(24*time.Hour+10*time.Minute))
	w.play(p, day.Add(30*time.Hour), day.Add(30*time.Hour+5*time.Minute))
	w.play(q, day.Add(30*time.Hour), day.Add(30*time.Hour+2*time.Minute))
	// A repeated connect while online is a refresh, not a session.
	if err := w.activity.Connect(ctx, w.guildID, w.serverID, q, day.Add(40*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := w.activity.Connect(ctx, w.guildID, w.serverID, q, day.Add(40*time.Hour+time.Second)); err != nil {
		t.Fatal(err)
	}

	type row struct {
		day      string
		seconds  int64
		sessions int
	}
	read := func(player int64) []row {
		rows, err := w.db.Pool.Query(ctx, `SELECT day::text, observed_seconds, sessions FROM player_daily_activity WHERE server_id=$1 AND player_id=$2 ORDER BY day`, w.serverID, player)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.day, &r.seconds, &r.sessions); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	got := read(p)
	want := []row{{"2026-06-10", 570, 1}, {"2026-06-11", 600 + 30 + 300, 1}}
	// 23:50:00 connect, checkpoints to 23:59:30 = 570 s on the 10th; the 00:00:00 checkpoint's 30 s
	// and the rest of the session (600 s) land on the 11th, plus the 300 s second session.
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("daily rows = %+v, want %+v", got, want)
	}
	var total int64
	if err := w.db.Pool.QueryRow(ctx, `SELECT total_observed_seconds FROM player_server_activity WHERE server_id=$1 AND player_id=$2`, w.serverID, p).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != got[0].seconds+got[1].seconds {
		t.Fatalf("daily seconds %d do not match the running total %d", got[0].seconds+got[1].seconds, total)
	}
	if other := read(q); len(other) != 1 || other[0].sessions != 2 || other[0].seconds != 120 {
		t.Fatalf("second player rows = %+v", other)
	}

	var peak int
	var seconds int64
	if err := w.db.Pool.QueryRow(ctx, `SELECT peak_players, player_seconds FROM server_hourly_activity WHERE server_id=$1 AND hour=$2`, w.serverID, day.Add(30*time.Hour)).Scan(&peak, &seconds); err != nil {
		t.Fatal(err)
	}
	// play() runs one player's whole session before the next, so the two were never sampled
	// together: Daily's checkpoints add 30..270 s and Other's 30..90 s.
	if peak != 1 || seconds != 270+90 {
		t.Fatalf("hourly peak=%d seconds=%d, want 1 and 360", peak, seconds)
	}
	// Two players connected at the same checkpoint are one sample with a peak of two.
	both := day.Add(50 * time.Hour)
	r, s := w.seedPlayer("R"), w.seedPlayer("S")
	for _, id := range []int64{r, s} {
		if err := w.activity.Connect(ctx, w.guildID, w.serverID, id, both); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.activity.CheckpointConnected(ctx, w.guildID, w.serverID, both.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := w.db.Pool.QueryRow(ctx, `SELECT peak_players, player_seconds FROM server_hourly_activity WHERE server_id=$1 AND hour=$2`, w.serverID, both).Scan(&peak, &seconds); err != nil {
		t.Fatal(err)
	}
	// Other is still connected from 40 h (no observation for 10 h, so no seconds), plus R and S.
	if peak != 3 || seconds != 60 {
		t.Fatalf("hourly peak=%d seconds=%d, want 3 and 60", peak, seconds)
	}
}

func TestBackfillDailyPresenceAddsOnlyMissingDays(t *testing.T) {
	w := newLifeWorld(t)
	ctx := context.Background()
	p, v := w.seedPlayer("Old"), w.seedPlayer("Victim")
	d1 := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	w.kill(p, v, d1, 50, false)
	w.kill(p, v, d1.Add(time.Hour), 60, false)
	w.locate(p, d1.Add(48*time.Hour), 1, 1)
	w.play(p, d1.Add(72*time.Hour), d1.Add(72*time.Hour+time.Minute))

	n, err := w.activity.BackfillDailyPresence(ctx, w.guildID, w.serverID)
	if err != nil {
		t.Fatal(err)
	}
	// Old: 1 March (kills) and 3 March (location); Victim: 1 March. 4 March is already OBSERVED.
	if n != 3 {
		t.Fatalf("backfilled %d days, want 3", n)
	}
	var observed, backfill int
	if err := w.db.Pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE source='OBSERVED'), COUNT(*) FILTER (WHERE source='BACKFILL') FROM player_daily_activity WHERE server_id=$1 AND player_id=$2`, w.serverID, p).Scan(&observed, &backfill); err != nil {
		t.Fatal(err)
	}
	if observed != 1 || backfill != 2 {
		t.Fatalf("observed=%d backfill=%d", observed, backfill)
	}
	if n, err := w.activity.BackfillDailyPresence(ctx, w.guildID, w.serverID); err != nil || n != 0 {
		t.Fatalf("second backfill added %d days, err=%v", n, err)
	}
	// A backfilled day the player is then observed on becomes OBSERVED.
	w.play(p, d1.Add(2*time.Hour), d1.Add(2*time.Hour+time.Minute))
	var source string
	if err := w.db.Pool.QueryRow(ctx, `SELECT source FROM player_daily_activity WHERE server_id=$1 AND player_id=$2 AND day='2026-03-01'`, w.serverID, p).Scan(&source); err != nil || source != "OBSERVED" {
		t.Fatalf("source=%q err=%v", source, err)
	}
}
