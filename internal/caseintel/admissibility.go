package caseintel

import (
 "errors"
 "math"
 "regexp"
 "sort"
 "strings"
 "time"
)

// AdmissibilitySample is a bounded projection of already persisted ADM evidence.
// SourceID is used only inside this audit and is never returned to clients.
type AdmissibilitySample struct {
 EvidenceID int64
 SourceID string
 SourceEndOffset int64
 LineSHA256 string
 EventType string
 ADMClock string
 X, Z *float64 // SUBJECT: retained for per-event coordinate completeness
 ActorX, ActorZ *float64
 TargetX, TargetZ *float64
}

// AdmissibilityReport reports source-data limitations, not suspicion or player risk.
// An observed coordinate pair is not a continuous movement sample.
type AdmissibilityReport struct {
 Mode string `json:"mode"`
 Coverage string `json:"coverage"`
 TimeBasis string `json:"timeBasis"`
 MovementDetectorStatus string `json:"movementDetectorStatus"`
 SafeSpeedPairs int `json:"safeSpeedPairs"`
 Enforcement string `json:"enforcement"`
 ObservationCount int `json:"observationCount"`
 SourceCount int `json:"sourceCount"`
 ValidSourceAddresses int `json:"validSourceAddresses"`
 InvalidSourceAddresses int `json:"invalidSourceAddresses"`
 OffsetHashCollisions int `json:"offsetHashCollisions"`
 DuplicateSourceAddresses int `json:"duplicateSourceAddresses"`
 ValidClockStrings int `json:"validClockStrings"`
 InvalidClockStrings int `json:"invalidClockStrings"`
 SameSecondAdjacent int `json:"sameSecondAdjacent"`
 ClockDecreasesInSource int `json:"clockDecreasesInSource"`
 CompleteCoordinatePairs int `json:"completeCoordinatePairs"`
 PartialCoordinatePairs int `json:"partialCoordinatePairs"`
 MissingCoordinatePairs int `json:"missingCoordinatePairs"`
 NonFiniteCoordinateValues int `json:"nonFiniteCoordinateValues"`
 // Additional per-role counts; no cross-role coordinate pair is ever formed.
 ActorCompleteCoordinatePairs int `json:"actorCompleteCoordinatePairs"`
 ActorPartialCoordinatePairs int `json:"actorPartialCoordinatePairs"`
 ActorMissingCoordinatePairs int `json:"actorMissingCoordinatePairs"`
 TargetCompleteCoordinatePairs int `json:"targetCompleteCoordinatePairs"`
 TargetPartialCoordinatePairs int `json:"targetPartialCoordinatePairs"`
 TargetMissingCoordinatePairs int `json:"targetMissingCoordinatePairs"`
 WindowTruncated bool `json:"windowTruncated"`
 Blockers []string `json:"blockers"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}package caseintel

import (
 "errors"
 "math"
 "regexp"
 "sort"
 "strings"
 "time"
)

// AdmissibilitySample is a bounded projection of already persisted ADM evidence.
// SourceID is used only inside this audit and is never returned to clients.
type AdmissibilitySample struct {
 EvidenceID int64
 SourceID string
 SourceEndOffset int64
 LineSHA256 string
 EventType string
 ADMClock string
 X, Z *float64 // SUBJECT: retained for per-event coordinate completeness
 ActorX, ActorZ *float64
 TargetX, TargetZ *float64
}

// AdmissibilityReport reports source-data limitations, not suspicion or player risk.
// An observed coordinate pair is not a continuous movement sample.
type AdmissibilityReport struct {
 Mode string `json:"mode"`
 Coverage string `json:"coverage"`
 TimeBasis string `json:"timeBasis"`
 MovementDetectorStatus string `json:"movementDetectorStatus"`
 SafeSpeedPairs int `json:"safeSpeedPairs"`
 Enforcement string `json:"enforcement"`
 ObservationCount int `json:"observationCount"`
 SourceCount int `json:"sourceCount"`
 ValidSourceAddresses int `json:"validSourceAddresses"`
 InvalidSourceAddresses int `json:"invalidSourceAddresses"`
 OffsetHashCollisions int `json:"offsetHashCollisions"`
 DuplicateSourceAddresses int `json:"duplicateSourceAddresses"`
 ValidClockStrings int `json:"validClockStrings"`
 InvalidClockStrings int `json:"invalidClockStrings"`
 SameSecondAdjacent int `json:"sameSecondAdjacent"`
 ClockDecreasesInSource int `json:"clockDecreasesInSource"`
 CompleteCoordinatePairs int `json:"completeCoordinatePairs"`
 PartialCoordinatePairs int `json:"partialCoordinatePairs"`
 MissingCoordinatePairs int `json:"missingCoordinatePairs"`
 NonFiniteCoordinateValues int `json:"nonFiniteCoordinateValues"`
 // Additional per-role counts; no cross-role coordinate pair is ever formed.
 ActorCompleteCoordinatePairs int `json:"actorCompleteCoordinatePairs"`
 ActorPartialCoordinatePairs int `json:"actorPartialCoordinatePairs"`
 ActorMissingCoordinatePairs int `json:"actorMissingCoordinatePairs"`
 TargetCompleteCoordinatePairs int `json:"targetCompleteCoordinatePairs"`
 TargetPartialCoordinatePairs int `json:"targetPartialCoordinatePairs"`
 TargetMissingCoordinatePairs int `json:"targetMissingCoordinatePairs"`
 WindowTruncated bool `json:"windowTruncated"`
 Blockers []string `json:"blockers"`
}

)

// A stored float8 NaN or infinity is not a usable ADM coordinate.
func finiteCoordinate(p *float64) bool {return p!=nil && !math.IsNaN(*p) && !math.IsInf(*p,0)}

// AuditAdmissibility examines only the supplied bounded records. It never
// equates byte-offset gaps with lost lines, or ADM clock deltas with elapsed
// gameplay time. Sorting is local to a source, solely for clock diagnostics.
func AuditAdmissibility(samples []AdmissibilitySample, limit int, truncated bool) (AdmissibilityReport,error) {
 if limit<1 || limit>500 || len(samples)>limit {return AdmissibilityReport{},errors.New("invalid bounded evidence sample")}
 report:=AdmissibilityReport{
  Mode:"SOURCE_QUALITY_ONLY",Coverage:"FILTERED_SOURCE_EVENTS_ONLY",
  TimeBasis:TimeBasis,MovementDetectorStatus:"BLOCKED",SafeSpeedPairs:0,
  Enforcement:"DISABLED",ObservationCount:len(samples),WindowTruncated:truncated,
  Blockers:[]string{"NO_DATED_HIGH_RESOLUTION_EVENT_TIME",
   "EVENT_TRIGGERED_POSITIONS_NOT_CONTINUOUS",
   "SOURCE_EMISSION_ORDER_NOT_GAMEPLAY_ORDER",
   "FILTERED_EVENTS_CANNOT_PROVE_SOURCE_COMPLETENESS",
   "EXCEPTION_MODEL_NOT_VALIDATED"},
 }
 if truncated {report.Blockers=append(report.Blockers,"BOUNDED_PAGE_EDGE")}
 sourceRecords:=make(map[string][]AdmissibilitySample)
 seen:=make(map[string]map[int64]string)
 for _,sample:=range samples {
  valid:=sample.EvidenceID>0 && sample.SourceID!="" && sample.SourceEndOffset>=0 &&
   sha256Hex.MatchString(sample.LineSHA256) && sample.EventType!=""
  if !valid {
   report.InvalidSourceAddresses++
  } else {
   report.ValidSourceAddresses++
   existing:=seen[sample.SourceID]
   if existing==nil {existing=make(map[int64]string);seen[sample.SourceID]=existing}
   normalizedHash:=strings.ToLower(sample.LineSHA256)
   if old,found:=existing[sample.SourceEndOffset];found {
    if old==normalizedHash {report.DuplicateSourceAddresses++} else {report.OffsetHashCollisions++}
   } else {existing[sample.SourceEndOffset]=normalizedHash}
  }
  if sample.SourceID!="" {sourceRecords[sample.SourceID]=append(sourceRecords[sample.SourceID],sample)}
  if len(sample.ADMClock)==8 {
   if _,err:=time.Parse("15:04:05",sample.ADMClock);err==nil {report.ValidClockStrings++} else {report.InvalidClockStrings++}
  } else {report.InvalidClockStrings++}
  // Count invalid numeric values per axis, independently of subject/actor/target.
  // Invalid values cannot turn a role into a complete coordinate pair.
  for _,axis:=range []*float64{sample.X,sample.Z,sample.ActorX,sample.ActorZ,sample.TargetX,sample.TargetZ}{
   if axis!=nil && !finiteCoordinate(axis) {report.NonFiniteCoordinateValues++}
  }
  sx,sz:=finiteCoordinate(sample.X),finiteCoordinate(sample.Z)
  ax,az:=finiteCoordinate(sample.ActorX),finiteCoordinate(sample.ActorZ)
  tx,tz:=finiteCoordinate(sample.TargetX),finiteCoordinate(sample.TargetZ)
  switch {
  case sx && sz:report.CompleteCoordinatePairs++
  case sx || sz:report.PartialCoordinatePairs++
  default:report.MissingCoordinatePairs++
  }
  switch {
  case ax && az:report.ActorCompleteCoordinatePairs++
  case ax || az:report.ActorPartialCoordinatePairs++
  default:report.ActorMissingCoordinatePairs++
  }
  switch {
  case tx && tz:report.TargetCompleteCoordinatePairs++
  case tx || tz:report.TargetPartialCoordinatePairs++
  default:report.TargetMissingCoordinatePairs++
  }
 }
 report.SourceCount=len(sourceRecords)
 if report.SourceCount>1 {report.Blockers=append(report.Blockers,"NO_CROSS_SOURCE_STITCH")}
 if report.InvalidSourceAddresses>0 {report.Blockers=append(report.Blockers,"INVALID_SOURCE_ADDRESS")}
 if report.OffsetHashCollisions>0 {report.Blockers=append(report.Blockers,"SOURCE_OFFSET_HASH_COLLISION")}
 if report.DuplicateSourceAddresses>0 {report.Blockers=append(report.Blockers,"DUPLICATE_SOURCE_ADDRESS_IN_SAMPLE")}
 if report.InvalidClockStrings>0 {report.Blockers=append(report.Blockers,"ADM_CLOCK_MISSING_OR_INVALID")}
 if report.PartialCoordinatePairs>0||report.ActorPartialCoordinatePairs>0||report.TargetPartialCoordinatePairs>0 {
  report.Blockers=append(report.Blockers,"PARTIAL_COORDINATE_PAIR")
 }
 if report.NonFiniteCoordinateValues>0 {report.Blockers=append(report.Blockers,"NON_FINITE_COORDINATE")}
 if report.ObservationCount==0 {report.Blockers=append(report.Blockers,"NO_OBSERVED_EVENTS")}
 for _,events:=range sourceRecords {
  sort.Slice(events,func(i,j int)bool {
   if events[i].SourceEndOffset==events[j].SourceEndOffset{return events[i].EvidenceID<events[j].EvidenceID}
   return events[i].SourceEndOffset<events[j].SourceEndOffset
  })
  previous:=""
  for _,ev:=range events {
   if len(ev.ADMClock)!=8 {previous="";continue}
   if _,err:=time.Parse("15:04:05",ev.ADMClock);err!=nil{previous="";continue}
   if previous!="" {
    if ev.ADMClock==previous {report.SameSecondAdjacent++}
    if ev.ADMClock<previous {report.ClockDecreasesInSource++}
   }
   previous=ev.ADMClock
  }
 }
 if report.ClockDecreasesInSource>0 {
  report.Blockers=append(report.Blockers,"CLOCK_DECREASE_OR_DAY_ROLLOVER_UNRESOLVED")
 }
 return report,nil
}
