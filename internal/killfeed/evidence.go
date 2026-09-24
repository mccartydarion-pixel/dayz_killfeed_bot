package killfeed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/yourname/dayz-killfeed/internal/repository"
)

// EvidenceStore records one source-addressed ADM line. Implementations must
// persist idempotently. The poller waits for acknowledgement before advancing
// its existing checkpoint, so a database outage cannot silently skip evidence.
type EvidenceStore interface {
	RecordCaseEvidence(context.Context, repository.CaseEvidenceInput) error
}

const caseEvidenceWriteTimeout = 5 * time.Second

// SetEvidenceStore is opt-in. Production enables it explicitly only when its
// migration has run. No extra Nitrado polling or Discord publish path exists.
func (e *Engine) SetEvidenceStore(store EvidenceStore) {
	if e != nil { e.evidenceStore = store }
}

func casePerson(ref *PlayerRef) repository.CaseEvidencePerson {
	if ref == nil { return repository.CaseEvidencePerson{} }
	out := repository.CaseEvidencePerson{DayZID:ref.ID,Name:ref.Name}
	if ref.Position != nil {
		x,z,y:=ref.Position.MapX(),ref.Position.MapZ(),ref.Position.Altitude()
		out.X,out.Z,out.Altitude=&x,&z,&y
	}
	return out
}

func caseBoundary(ev *Event) string {
	if ev == nil { return "" }
	switch ev.Type {
	case EventPlayerConnect: return "CONNECT"
	case EventPlayerDisconnect: return "DISCONNECT"
	case EventPlayerRespawn: return "RESPAWN"
	case EventPlayerDeath,EventPlayerKill: return "DEATH"
	case EventSuicideAction: return "SUICIDE"
	default: return ""
	}
}

func caseEvidenceCandidate(t EventType) bool {
	switch t {
	case EventPlayerConnect,EventPlayerDisconnect,EventPlayerRespawn,
		EventPlayerDeath,EventPlayerKill,EventPlayerHit,EventPlayerUnconscious,
		EventPlayerConscious,EventSuicideAction:
		return true
	default:
		return false
	}
}

func caseEvidenceInput(ev *Event, guildID,serverID int64,sourcePath string,endOffset int64) repository.CaseEvidenceInput {
	sum:=sha256.Sum256([]byte(ev.Raw))
	actor:=ev.Attacker
	if actor==nil {actor=ev.Killer}
	return repository.CaseEvidenceInput{
		GuildID:guildID,ServerID:serverID,SourceID:canonicalADMID(sourcePath),
		SourceEndOffset:endOffset,LineSHA256:hex.EncodeToString(sum[:]),
		EventType:string(ev.Type),ADMClock:ev.TimeOfDay,
		Subject:casePerson(ev.Player),Actor:casePerson(actor),Target:casePerson(ev.Victim),
		Weapon:ev.Weapon,Ammo:ev.Ammo,HitZone:ev.HitZone,HitZoneID:ev.HitZoneID,
		Damage:ev.Damage,HP:ev.HP,DistanceMeters:ev.Distance,BoundaryKind:caseBoundary(ev),
	}
}

// observeEvidence runs BEFORE legacy semantic dedupe. Two physically distinct
// hits can have identical second/damage/zone/weapon: different byte offsets
// preserve both in evidence while the existing hitfeed behavior stays as-is.
func (e *Engine) observeEvidence(ev *Event, sourcePath string, endOffset int64) error {
	if e == nil || e.evidenceStore == nil || ev == nil || !caseEvidenceCandidate(ev.Type) { return nil }
	if e.guildID<=0 || e.serverID<=0 || sourcePath=="" || endOffset<0 {
		return errors.New("C.A.S.E. evidence missing scoped source address")
	}
	ctx,cancel:=context.WithTimeout(context.Background(),caseEvidenceWriteTimeout)
	defer cancel()
	return e.evidenceStore.RecordCaseEvidence(ctx,caseEvidenceInput(ev,e.guildID,e.serverID,sourcePath,endOffset))
}
