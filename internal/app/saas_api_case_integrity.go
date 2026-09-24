package app

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "log/slog"
 "net/http"
 "time"

 "github.com/jackc/pgx/v5"
 "github.com/yourname/dayz-killfeed/internal/killfeed"
 "github.com/yourname/dayz-killfeed/internal/permissions"
)

// C.A.S.E. Phase 2E is a read-only view of the already-running ADM worker.
// No extra Nitrado requests, second poller, or new source authority are added.
type caseSourceIntegrity struct {
 Mode string `json:"mode"`
 ServerID int64 `json:"serverId"`
 GeneratedAt time.Time `json:"generatedAt"`
 WorkerAvailable bool `json:"workerAvailable"`
 SourceState string `json:"sourceState"`
 SourceReason string `json:"sourceReason"`
 SelectedSourceRef *string `json:"selectedSourceRef"`
 AcceptedSourceRef *string `json:"acceptedSourceRef"`
 LatestListedSourceRef *string `json:"latestListedSourceRef"`
 SelectedIsAccepted *bool `json:"selectedIsAccepted"`
 LastPollAt *time.Time `json:"lastPollAt"`
 LastSourceChangeAt *time.Time `json:"lastSourceChangeAt"`
 LastMetadataCheckAt *time.Time `json:"lastMetadataCheckAt"`
 RemoteBytes *int64 `json:"remoteBytes"`
 CheckpointBytes *int64 `json:"checkpointBytes"`
 CheckpointSavedAt *time.Time `json:"checkpointSavedAt"`
 LastDownloadAt *time.Time `json:"lastDownloadAt"`
 TransportFailureStreak *int `json:"transportFailureStreak"`
 LastFailureStage string `json:"lastFailureStage"`
 LastFailureAt *time.Time `json:"lastFailureAt"`
 LatestEvidenceIngestedAt *time.Time `json:"latestEvidenceIngestedAt"`
 LatestEvidenceSourceRef *string `json:"latestEvidenceSourceRef"`
 LatestEvidenceOffset *int64 `json:"latestEvidenceOffset"`
 EvidenceLines24h int64 `json:"evidenceLines24h"`
 CollectorConfigured bool `json:"collectorConfigured"`
 Coverage string `json:"coverage"`
 ElapsedTimeTrusted bool `json:"elapsedTimeTrusted"`
 MovementDetectorStatus string `json:"movementDetectorStatus"`
 DetectorsEnabled bool `json:"detectorsEnabled"`
 Enforcement string `json:"enforcement"`
}

func caseSourceRef(value string) *string {
 if value=="" {return nil}
 hash:=sha256.Sum256([]byte(value))
 ref:=hex.EncodeToString(hash[:])[:16]
 return &ref
}
func caseOptionalTime(t time.Time)*time.Time {
 if t.IsZero(){return nil};v:=t.UTC();return &v
}
func caseSourceSnapshot(serverID int64,now time.Time,source killfeed.ADMSourceHealth,
 pipeline killfeed.RuntimeDiagnosticSnapshot, available bool) caseSourceIntegrity {
 out:=caseSourceIntegrity{
  Mode:"OBSERVATION_ONLY",ServerID:serverID,GeneratedAt:now.UTC(),
  WorkerAvailable:available,SourceState:"UNAVAILABLE",SourceReason:"no running ADM worker snapshot",
  Coverage:"SELECTED_ADM_EVENTS_ONLY",ElapsedTimeTrusted:false,
  MovementDetectorStatus:"BLOCKED",DetectorsEnabled:false,Enforcement:"DISABLED",
 }
 if !available{return out}
 out.SourceState,out.SourceReason=killfeed.ClassifyADMSourceHealth(source,now)
 out.SelectedSourceRef=caseSourceRef(source.SelectedFile)
 out.AcceptedSourceRef=caseSourceRef(source.AcceptedFile)
 out.LatestListedSourceRef=caseSourceRef(source.NewestListedFile)
 if source.SelectedFile!=""&&source.AcceptedFile!=""{
  matched:=source.SelectedFile==source.AcceptedFile;out.SelectedIsAccepted=&matched
 }
 out.LastPollAt=caseOptionalTime(source.LastCycleAt)
 out.LastSourceChangeAt=caseOptionalTime(source.LastChangeAt)
 out.LastMetadataCheckAt=caseOptionalTime(pipeline.LastMetadataCheck)
 out.CheckpointSavedAt=caseOptionalTime(pipeline.CheckpointLastSaved)
 out.LastDownloadAt=caseOptionalTime(pipeline.LastDownloadSuccess)
 out.LastFailureAt=caseOptionalTime(pipeline.LastErrorAt)
 out.LastFailureStage=pipeline.LastErrorStage
 streak:=source.TransportStreak;out.TransportFailureStreak=&streak
 // Zero is a genuine zero ONLY for a live worker with a source/metadata
 // snapshot. Otherwise nil prevents a misleading zero-byte source claim.
 if source.SelectedFile!=""&&!pipeline.LastMetadataCheck.IsZero(){
  remote:=pipeline.RemoteSize;out.RemoteBytes=&remote
 }
 if source.SelectedFile!=""&&!pipeline.CheckpointLastSaved.IsZero(){
  offset:=pipeline.CheckpointOffset;out.CheckpointBytes=&offset
 }
 return out
}

func (a *App) handleAntiCheatIntegrity(w http.ResponseWriter,r *http.Request) {
 ac,ok:=a.requireCapability(w,r,permissions.CapPlayerLocationView)
 if !ok{return}
 if ac.scope.ServerID==nil {writeSaaSError(w,codeInvalidRequest,"no DayZ server selected");return}
 if a.DB==nil||a.DB.Pool==nil {writeSaaSError(w,codeInternalError,"C.A.S.E. source health unavailable");return}
 if !enforceRateLimit(w,a.saasAdminReadLimiter,rateLimitKey(r)){return}
 serverID:=*ac.scope.ServerID
 now:=time.Now().UTC()
 // Engine pointers are obtained only from this server ID. SourceHealth and
 // Diagnostics each return a synchronized copy. They are not an atomic pair;
 // never infer exact cross-snapshot byte consistency or a contiguous stream.
 a.presenceMu.Lock()
 engine:=a.presenceEngines[serverID]
 a.presenceMu.Unlock()
 available:=engine!=nil
 var source killfeed.ADMSourceHealth
 var pipeline killfeed.RuntimeDiagnosticSnapshot
 if available {
  source=engine.SourceHealth()
  if diag:=engine.Diagnostics();diag!=nil {pipeline=diag.Snapshot()}
 }
 out:=caseSourceSnapshot(serverID,now,source,pipeline,available)
 out.CollectorConfigured=caseEvidenceEnabledForServer(serverID)
 ctx,cancel:=context.WithTimeout(r.Context(),adminTimeout)
 defer cancel()
 var sourceID *string
 err:=a.DB.Pool.QueryRow(ctx,`
  SELECT COUNT(*) FROM case_evidence_events
  WHERE guild_id=$1 AND server_id=$2 AND ingested_at >= $3 AND ingested_at <= $4
 `,ac.scope.GuildID,serverID,now.Add(-24*time.Hour),now).
 Scan(&out.EvidenceLines24h)
 if err!=nil {
  slog.Warn("component=case","event","source_integrity_count_failed","err",err.Error())
  writeSaaSError(w,codeInternalError,"could not read C.A.S.E. source integrity");return
 }
 // A single row supplies ingestion time, source and offset so they never
 // appear as though they came from different evidence records.
 err=a.DB.Pool.QueryRow(ctx,`
  SELECT source_id,source_end_offset,ingested_at FROM case_evidence_events
  WHERE guild_id=$1 AND server_id=$2 ORDER BY id DESC LIMIT 1
 `,ac.scope.GuildID,serverID).Scan(&sourceID,&out.LatestEvidenceOffset,&out.LatestEvidenceIngestedAt)
 if err!=nil && err!=pgx.ErrNoRows {
  slog.Warn("component=case","event","source_integrity_latest_failed","err",err.Error())
  writeSaaSError(w,codeInternalError,"could not read latest C.A.S.E. source");return
 }
 if sourceID!=nil{out.LatestEvidenceSourceRef=caseSourceRef(*sourceID)}
 a.recordAudit(ctx,ac,"CASE_SOURCE_INTEGRITY_VIEWED","","","success",nil,
  map[string]any{"workerAvailable":available,"sourceState":out.SourceState})
 writeSaaSJSON(w,http.StatusOK,out)
}
