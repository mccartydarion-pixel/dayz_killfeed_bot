package killfeed

import (
	"math"
	"strings"
)

// Final-hit correlation (Embed Designer: hit data on kills). A DayZ ADM kill line ("... killed by
// Player ...") carries no hit zone or damage. DayZ writes the lethal hit on the line IMMEDIATELY
// before it - observed on every Champions kill of 2026-09-24:
//
//	09:33:17 | Player "V" (DEAD) (id=… pos=<…>)[HP: 0] hit by Player "K" (id=…) into Head(0) for 22.0527 damage (Bullet_556x45) with M4-A1 from 74.9605 meters
//	09:33:17 | Player "V" (DEAD) (id=… pos=<…>) killed by Player "K" (id=…) with M4-A1 from 74.9605 meters
//
// A kill is given its final hit ONLY when all of this evidence agrees; anything less leaves it
// without hit data (the custom-embed variables are then simply absent):
//
//   - the hit is marked lethal: the victim carries the (DEAD) marker;
//   - same physical source: the same canonical ADM file (so the same boot session), and the hit is
//     the line immediately preceding the kill line (its end offset is the kill line's start);
//   - same players: the hit's victim and attacker ids are the kill's victim and killer ids;
//   - same moment: the same HH:MM:SS;
//   - same shot: the same weapon and the same distance, when both lines state them.
//
// The match lives on Event.FinalHit, NOT on Event.HitZone/Damage: those fields feed headshot
// statistics, the default card and the durable kill fingerprint, which must not change. Nothing
// here alters hit processing, the Hitfeed, dedupe, persistence or checkpoints.

// FinalHit is the lethal hit reliably correlated with a kill.
type FinalHit struct {
	Zone   string
	ZoneID string
	Damage *float64
}

type lethalHit struct {
	file        string
	end         int64
	timeOfDay   string
	victimID    string
	attackerID  string
	weapon      string
	distance    *float64
	zone, zonID string
	damage      *float64
}

// noteLine records the physical position of every processed line: the previous line's end is
// what "immediately preceding" means.
func (e *Engine) noteLine(file string, end int64) (prevFile string, prevEnd int64) {
	prevFile, prevEnd = e.lastLineFile, e.lastLineEnd
	e.lastLineFile, e.lastLineEnd = file, end
	return prevFile, prevEnd
}

// correlateFinalHit is called for every parsed event with its physical source.
func (e *Engine) correlateFinalHit(ev *Event, file string, end int64, prevFile string, prevEnd int64) {
	if ev == nil || file == "" {
		return
	}
	switch ev.Type {
	case EventPlayerHit:
		if !ev.Dead || ev.Victim == nil || ev.Attacker == nil || ev.Victim.ID == "" || ev.Attacker.ID == "" || ev.HitZone == "" {
			return
		}
		// Only the most recent lethal hit is kept: a kill can only match the line right before it.
		e.lastLethal = &lethalHit{file: file, end: end, timeOfDay: ev.TimeOfDay, victimID: ev.Victim.ID, attackerID: ev.Attacker.ID,
			weapon: strings.TrimSpace(ev.Weapon), distance: ev.Distance, zone: ev.HitZone, zonID: ev.HitZoneID, damage: ev.Damage}
	case EventPlayerKill:
		h := e.lastLethal
		e.lastLethal = nil // single use: never reused for a later kill
		if h == nil || ev.Victim == nil || ev.Killer == nil {
			return
		}
		switch {
		case h.file != file || prevFile != file || h.end != prevEnd: // not the immediately preceding line of this file
			return
		case h.victimID != ev.Victim.ID || h.attackerID != ev.Killer.ID:
			return
		case h.timeOfDay == "" || h.timeOfDay != ev.TimeOfDay:
			return
		case h.weapon != "" && strings.TrimSpace(ev.Weapon) != "" && !strings.EqualFold(h.weapon, strings.TrimSpace(ev.Weapon)):
			return
		case h.distance != nil && ev.Distance != nil && math.Abs(*h.distance-*ev.Distance) > 0.001:
			return
		}
		fh := &FinalHit{Zone: h.zone, ZoneID: h.zonID}
		if h.damage != nil {
			d := *h.damage
			fh.Damage = &d
		}
		ev.FinalHit = fh
	default:
		// Any other line between a lethal hit and a kill already breaks adjacency (prevEnd).
	}
}
