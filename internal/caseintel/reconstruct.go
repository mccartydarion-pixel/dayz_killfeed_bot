// Package caseintel reconstructs bounded player observation windows from
// durable ADM source lines. It does not infer cheating, UTC event times,
// online duration, speed or client input telemetry.
package caseintel

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

const TimeBasis = "SOURCE_BYTE_OFFSET_WITHIN_FILE"

// Event is an internal, scoped repository projection. SourceID is not exposed
// to the API, and cannot be substituted by ingestion order or ADM HH:MM:SS.
type Event struct {
	ID int64
	SourceID string
	SourceEndOffset int64
	Type string
	ADMClock string
	IngestedAt time.Time
	SubjectID, ActorID, TargetID *int64
	SubjectName, ActorName, TargetName string
	SubjectX, SubjectZ, SubjectAltitude *float64
	ActorX, ActorZ, ActorAltitude *float64
	TargetX, TargetZ, TargetAltitude *float64
}

type Position struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
	Altitude *float64 `json:"altitude"`
}

type Observation struct {
	EvidenceID int64 `json:"evidenceId"`
	SourceEndOffset int64 `json:"sourceEndOffset"`
	Type string `json:"type"`
	Role string `json:"role"`
	Name string `json:"name"`
	ADMClock string `json:"admClock"`
	IngestedAt time.Time `json:"ingestedAt"`
	Position *Position `json:"position"`
	Notes []string `json:"notes"`
}

// LifeWindow is a sequence of observations between recorded life markers;
// FIRST_OBSERVED and UNOBSERVED_END are deliberately NOT assertions of birth,
// death, respawn or continuous presence.
type LifeWindow struct {
	StartOffset int64 `json:"startOffset"`
	EndOffset int64 `json:"endOffset"`
	StartReason string `json:"startReason"`
	EndReason string `json:"endReason"`
	EvidenceIDs []int64 `json:"evidenceIds"`
}

type ConnectionWindow struct {
	ID string `json:"id"`
	StartOffset int64 `json:"startOffset"`
	EndOffset int64 `json:"endOffset"`
	StartReason string `json:"startReason"`
	EndReason string `json:"endReason"`
	Flags []string `json:"flags"`
	Observations []Observation `json:"observations"`
	Lives []LifeWindow `json:"lives"`
}

type SourceWindow struct {
	SourceRef string `json:"sourceRef"`
	ObservationCount int `json:"observationCount"`
	ConnectionWindows []ConnectionWindow `json:"connectionWindows"`
}

// Reconstruction is a read-only bounded projection; it is never a cheat
// verdict. The source groups are ordered by newest ingestion ID for display,
// while events within each source are ordered exclusively by byte offset.
type Reconstruction struct {
	Mode string `json:"mode"`
	TimeBasis string `json:"timeBasis"`
	PlayerID int64 `json:"playerId"`
	ObservationLimit int `json:"observationLimit"`
	WindowTruncated bool `json:"windowTruncated"`
	NoCrossSourceStitch bool `json:"noCrossSourceStitch"`
	NoDurationInference bool `json:"noDurationInference"`
	Sources []SourceWindow `json:"sources"`
	Quality QualityReport `json:"quality"`
	DetectorsEnabled bool `json:"detectorsEnabled"`
	Enforcement string `json:"enforcement"`
}

func ref(source string) string {
	hash := sha256.Sum256([]byte(source))
	return hex.EncodeToString(hash[:])[:16]
}

func matches(id *int64, playerID int64) bool { return id != nil && *id == playerID }
func position(x,z,y *float64) *Position {
	if x == nil || z == nil { return nil }
	return &Position{X:*x,Z:*z,Altitude:y}
}
func project(e Event, playerID int64) Observation {
	o:=Observation{
		EvidenceID:e.ID,SourceEndOffset:e.SourceEndOffset,Type:e.Type,
		ADMClock:e.ADMClock,IngestedAt:e.IngestedAt,Notes:make([]string,0),
	}
	// The victim's role must take precedence on a self-hit or self-kill.
	switch {
	case matches(e.TargetID,playerID):
		o.Role="TARGET";o.Name=e.TargetName
		o.Position=position(e.TargetX,e.TargetZ,e.TargetAltitude)
	case matches(e.SubjectID,playerID):
		o.Role="SUBJECT";o.Name=e.SubjectName
		o.Position=position(e.SubjectX,e.SubjectZ,e.SubjectAltitude)
	case matches(e.ActorID,playerID):
		o.Role="ACTOR";o.Name=e.ActorName
		o.Position=position(e.ActorX,e.ActorZ,e.ActorAltitude)
	}
	return o
}
func isConnect(o Observation) bool { return o.Type=="PLAYER_CONNECT" && o.Role=="SUBJECT" }
func isDisconnect(o Observation) bool { return o.Type=="PLAYER_DISCONNECT" && o.Role=="SUBJECT" }
func isRespawn(o Observation) bool { return o.Type=="PLAYER_RESPAWN" && o.Role=="SUBJECT" }
func isDeath(o Observation) bool {
	return (o.Type=="PLAYER_KILL" && o.Role=="TARGET") ||
		(o.Type=="PLAYER_DEATH" && o.Role=="SUBJECT")
}
func flagOnce(w *ConnectionWindow, flag string) {
	for _, f := range w.Flags {if f==flag{return}}
	w.Flags=append(w.Flags,flag)
}
func newWindow(sourceRef string, first Observation, explicit bool) *ConnectionWindow {
	reason:="FIRST_OBSERVED"
	if explicit {reason="CONNECT"}
	w:=&ConnectionWindow{
		ID:fmt.Sprintf("%s:%d",sourceRef,first.SourceEndOffset),
		StartOffset:first.SourceEndOffset,EndOffset:first.SourceEndOffset,
		StartReason:reason,EndReason:"UNOBSERVED_END",
		Flags:make([]string,0),Observations:make([]Observation,0),Lives:make([]LifeWindow,0),
	}
	if !explicit {flagOnce(w,"MISSING_CONNECT_IN_WINDOW")}
	return w
}

// Reconstruct uses the identity-selected rows returned by the repository.
// Ingestion timestamps and ADM clocks are displayed, NEVER used for sorting.
// No connection/life episode crosses an ADM source file, even if the file
// names, clock readings or ingestion times look contiguous.
func Reconstruct(playerID int64, events []Event, limit int, truncated bool) Reconstruction {
	result:=Reconstruction{
		Mode:"RECONSTRUCTION_ONLY",TimeBasis:TimeBasis,PlayerID:playerID,
		ObservationLimit:limit,WindowTruncated:truncated,
		NoCrossSourceStitch:true,NoDurationInference:true,
		Sources:make([]SourceWindow,0),DetectorsEnabled:false,Enforcement:"DISABLED",
	}
	type group struct{source string;latestID int64;items []Event}
	groups:=map[string]*group{}
	for _,e:=range events {
		if e.SourceID=="" || e.ID<=0 {continue}
		if !matches(e.SubjectID,playerID)&&!matches(e.ActorID,playerID)&&!matches(e.TargetID,playerID){continue}
		g:=groups[e.SourceID]
		if g==nil {g=&group{source:e.SourceID,items:make([]Event,0)};groups[e.SourceID]=g}
		g.items=append(g.items,e)
		if e.ID>g.latestID {g.latestID=e.ID}
	}
	ordered:=make([]*group,0,len(groups))
	for _,g:=range groups {ordered=append(ordered,g)}
	sort.Slice(ordered,func(i,j int)bool{
		if ordered[i].latestID==ordered[j].latestID{return ordered[i].source<ordered[j].source}
		return ordered[i].latestID>ordered[j].latestID
	})
	for _,g:=range ordered {
		sort.Slice(g.items,func(i,j int)bool{
			if g.items[i].SourceEndOffset==g.items[j].SourceEndOffset{return g.items[i].ID<g.items[j].ID}
			return g.items[i].SourceEndOffset<g.items[j].SourceEndOffset
		})
		source:=SourceWindow{SourceRef:ref(g.source),ObservationCount:len(g.items),
			ConnectionWindows:make([]ConnectionWindow,0)}
		var current *ConnectionWindow
		var life *LifeWindow
		deadRecorded:=false
		closeLife:=func(end string, offset int64){
			if life==nil{return}
			life.EndOffset=offset;life.EndReason=end
			current.Lives=append(current.Lives,*life)
			life=nil
		}
		closeWindow:=func(end string,offset int64){
			if current==nil{return}
			if life!=nil {closeLife("UNOBSERVED_LIFE_END",offset)}
			current.EndOffset=offset;current.EndReason=end
			if end=="UNOBSERVED_END"{flagOnce(current,"MISSING_DISCONNECT_IN_WINDOW")}
			source.ConnectionWindows=append(source.ConnectionWindows,*current)
			current=nil;deadRecorded=false
		}
		for _,e:=range g.items {
			o:=project(e,playerID)
			if isConnect(o) && current!=nil {
				flagOnce(current,"REPEATED_CONNECT_WITHOUT_DISCONNECT")
				closeWindow("REPEATED_CONNECT",current.EndOffset)
			}
			if current==nil {current=newWindow(source.SourceRef,o,isConnect(o));deadRecorded=false}
			current.Observations=append(current.Observations,o)
			current.EndOffset=o.SourceEndOffset
			ob:=&current.Observations[len(current.Observations)-1]
			if isRespawn(o) {
				if life!=nil {
					flagOnce(current,"RESPAWN_WITHOUT_RECORDED_DEATH")
					closeLife("RESPAWN_WITHOUT_RECORDED_DEATH",o.SourceEndOffset)
				}
				life=&LifeWindow{StartOffset:o.SourceEndOffset,EndOffset:o.SourceEndOffset,
					StartReason:"RECORDED_RESPAWN",EndReason:"UNOBSERVED_LIFE_END",
					EvidenceIDs:make([]int64,0)}
				deadRecorded=false
			}
			if isDeath(o) {
				if life==nil && !deadRecorded {
					life=&LifeWindow{StartOffset:o.SourceEndOffset,EndOffset:o.SourceEndOffset,
						StartReason:"FIRST_OBSERVED",EndReason:"UNOBSERVED_LIFE_END",
						EvidenceIDs:make([]int64,0)}
				}
				if life!=nil {
					life.EvidenceIDs=append(life.EvidenceIDs,o.EvidenceID)
					closeLife("RECORDED_DEATH",o.SourceEndOffset)
				} else {
					ob.Notes=append(ob.Notes,"REPEATED_DEATH_IN_SOURCE_ORDER")
					flagOnce(current,"REPEATED_DEATH_IN_SOURCE_ORDER")
				}
				deadRecorded=true
			} else if isDisconnect(o) {
				if life!=nil {life.EvidenceIDs=append(life.EvidenceIDs,o.EvidenceID)}
				closeWindow("DISCONNECT",o.SourceEndOffset)
			} else {
				if !isConnect(o) && !isRespawn(o) && deadRecorded {
					ob.Notes=append(ob.Notes,"OBSERVATION_AFTER_RECORDED_DEATH_IN_SOURCE_ORDER")
					flagOnce(current,"OBSERVATION_AFTER_RECORDED_DEATH_IN_SOURCE_ORDER")
				}
				if life==nil && !deadRecorded && !isConnect(o) {
					life=&LifeWindow{StartOffset:o.SourceEndOffset,EndOffset:o.SourceEndOffset,
						StartReason:"FIRST_OBSERVED",EndReason:"UNOBSERVED_LIFE_END",
						EvidenceIDs:make([]int64,0)}
				}
				if life!=nil {life.EvidenceIDs=append(life.EvidenceIDs,o.EvidenceID)}
				if o.Type=="SUICIDE_ACTION" {
					// The ADM line proves the action, not death. Do not close life.
					ob.Notes=append(ob.Notes,"SUICIDE_ACTION_NOT_PROOF_OF_DEATH")
				}
			}
		}
		if current!=nil {closeWindow("UNOBSERVED_END",current.EndOffset)}
		result.Sources=append(result.Sources,source)
	}
	result.Quality=AssessQuality(result)
	return result
}
