package caseintel

import (
 "errors"
 "regexp"
 "sort"
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
 X, Z *float64
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
 WindowTruncated bool `json:"windowTruncated"`
 Blockers []string `json:"blockers"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

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
   if old,found:=existing[sample.SourceEndOffset];found {
    if old==sample.LineSHA256 {report.DuplicateSourceAddresses++} else {report.OffsetHashCollisions++}
   } else {existing[sample.SourceEndOffset]=sample.LineSHA256}
  }
  if sample.SourceID!="" {sourceRecords[sample.SourceID]=append(sourceRecords[sample.SourceID],sample)}
  if len(sample.ADMClock)==8 {
   if _,err:=time.Parse("15:04:05",sample.ADMClock);err==nil {report.ValidClockStrings++} else {report.InvalidClockStrings++}
  } else {report.InvalidClockStrings++}
  switch {
  case sample.X!=nil && sample.Z!=nil:report.CompleteCoordinatePairs++
  case sample.X!=nil || sample.Z!=nil:report.PartialCoordinatePairs++
  default:report.MissingCoordinatePairs++
  }
 }
 report.SourceCount=len(sourceRecords)
 if report.SourceCount>1 {report.Blockers=append(report.Blockers,"NO_CROSS_SOURCE_STITCH")}
 if report.InvalidSourceAddresses>0 {report.Blockers=append(report.Blockers,"INVALID_SOURCE_ADDRESS")}
 if report.OffsetHashCollisions>0 {report.Blockers=append(report.Blockers,"SOURCE_OFFSET_HASH_COLLISION")}
 if report.DuplicateSourceAddresses>0 {report.Blockers=append(report.Blockers,"DUPLICATE_SOURCE_ADDRESS_IN_SAMPLE")}
 if report.InvalidClockStrings>0 {report.Blockers=append(report.Blockers,"ADM_CLOCK_MISSING_OR_INVALID")}
 if report.PartialCoordinatePairs>0 {report.Blockers=append(report.Blockers,"PARTIAL_COORDINATE_PAIR")}
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
