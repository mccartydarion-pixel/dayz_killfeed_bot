package caseintel

import "errors"

// HitKillQuality is a bounded, synthetic/source-quality-only observation.
// It cannot infer aim automation, recoil scripting, cheating, or player risk.
// A hit/kill count never grants detector admission.
type HitKillQuality struct {
 Mode string
 HitObservations int
 KillObservations int
 OtherObservations int
 SourceCount int
 InvalidSourceAddresses int
 DuplicateSourceAddresses int
 OffsetHashCollisions int
 InvalidClockStrings int
 WindowTruncated bool
 Limitations []string
 DetectorStatus string
 FindingsEnabled bool
 Enforcement string
}

// AuditHitKillQuality restricts the projection to the existing retained
// source-addressed events. No identity, gameplay duration or weapon inference
// is returned. Non-hit/kill rows are counted only as contextual observations.
func AuditHitKillQuality(samples []AdmissibilitySample, limit int, truncated bool)(HitKillQuality,error){
 if limit<1||limit>200||len(samples)>limit{return HitKillQuality{},errors.New("invalid bounded hit/kill sample")}
 audit,err:=AuditAdmissibility(samples,limit,truncated)
 if err!=nil{return HitKillQuality{},err}
 result:=HitKillQuality{
  Mode:"SOURCE_QUALITY_ONLY",
  HitObservations:audit.HitObservations,
  KillObservations:audit.KillObservations,
  OtherObservations:audit.ObservationCount-audit.HitObservations-audit.KillObservations,
  SourceCount:audit.SourceCount,
  InvalidSourceAddresses:audit.InvalidSourceAddresses,
  DuplicateSourceAddresses:audit.DuplicateSourceAddresses,
  OffsetHashCollisions:audit.OffsetHashCollisions,
  InvalidClockStrings:audit.InvalidClockStrings,
  WindowTruncated:audit.WindowTruncated,
  DetectorStatus:"NOT_VALIDATED",
  FindingsEnabled:false,
  Enforcement:"DISABLED",
  Limitations:[]string{
   "FILTERED_RETENTION_NOT_FULL_ADM_COVERAGE",
   "NO_VERIFIED_HIT_KILL_EVENT_SEMANTICS",
   "NO_VALIDATED_FALSE_POSITIVE_MODEL",
   "COUNT_IS_NOT_A_CHEATING_SIGNAL",
   "NO_TRUSTED_GAMEPLAY_ELAPSED_TIME",
  },
 }
 if len(samples)==0{result.Limitations=append(result.Limitations,"NO_OBSERVED_EVENTS")}
 if truncated{result.Limitations=append(result.Limitations,"BOUNDED_PAGE_EDGE")}
 if audit.SourceCount>1{result.Limitations=append(result.Limitations,"MULTIPLE_INDEPENDENT_ADM_SOURCES")}
 if audit.InvalidSourceAddresses>0||audit.OffsetHashCollisions>0{
  result.Limitations=append(result.Limitations,"SOURCE_PROVENANCE_UNVERIFIED")
 }
 if audit.InvalidClockStrings>0{result.Limitations=append(result.Limitations,"CLOCK_ONLY_OR_INVALID")}
 return result,nil
}
