package repository

import (
 "context"
 "crypto/sha256"
 "encoding/hex"
 "errors"
 "strings"
 "time"

 "github.com/jackc/pgx/v5"
 "github.com/jackc/pgx/v5/pgxpool"
)

// CurrentSourceEvidenceSnapshot is an offline, bounded read-only observation.
// It is not a release-gate PASS, source-completeness assertion or finding.
// No canonical source path, player data or raw ADM text leaves the repository.
type CurrentSourceEvidenceSnapshot struct {
 SelectedSourceRef string
 LatestRetainedSourceRef string
 LatestRetainedMatchesSelected bool
 SelectedSourceObservations int
 SelectedSourceWindowTruncated bool
 LatestSelectedOffset *int64
 LatestSelectedIngestedAt *time.Time
 LatestRetainedOffset *int64
 LatestRetainedIngestedAt *time.Time
}

// CurrentSourceEvidenceInspector has no runtime caller. The source identity
// must come from the existing trusted worker, not a client-supplied pseudonym.
type CurrentSourceEvidenceInspector struct { pool *pgxpool.Pool }

func NewCurrentSourceEvidenceInspector(pool *pgxpool.Pool) *CurrentSourceEvidenceInspector {
 return &CurrentSourceEvidenceInspector{pool:pool}
}

func currentSourceRef(source string) string {
 sum:=sha256.Sum256([]byte(source))
 return hex.EncodeToString(sum[:])[:16]
}

// Inspect reads the latest retained row and up to limit+1 selected-source rows
// from the SAME repeatable-read, read-only snapshot and exact guild/server.
// A selected source can have historical rows while a newer retained record
// belongs to another boot; only the latter determines latest-source matching.
// A bounded sample is not proof that every ADM line was retained.
func (r *CurrentSourceEvidenceInspector) Inspect(ctx context.Context,
 guildID,serverID int64,selectedCanonicalSource string,limit int)(CurrentSourceEvidenceSnapshot,error) {
 if r==nil||r.pool==nil {return CurrentSourceEvidenceSnapshot{},errors.New("C.A.S.E. repository unavailable")}
 if guildID<=0||serverID<=0||strings.TrimSpace(selectedCanonicalSource)==""||
  selectedCanonicalSource!=strings.TrimSpace(selectedCanonicalSource)||
  limit<1||limit>200 {
  return CurrentSourceEvidenceSnapshot{},errors.New("invalid selected source scope or bound")
 }
 tx,err:=r.pool.BeginTx(ctx,pgx.TxOptions{IsoLevel:pgx.RepeatableRead,AccessMode:pgx.ReadOnly})
 if err!=nil{return CurrentSourceEvidenceSnapshot{},errors.New("read-only source inspection unavailable")}
 defer tx.Rollback(ctx)
 out:=CurrentSourceEvidenceSnapshot{SelectedSourceRef:currentSourceRef(selectedCanonicalSource)}
 var latestSource string
 var latestOffset int64
 var latestTime time.Time
 err=tx.QueryRow(ctx,`SELECT source_id,source_end_offset,ingested_at
 FROM case_evidence_events WHERE guild_id=$1 AND server_id=$2
 ORDER BY id DESC LIMIT 1`,guildID,serverID).Scan(&latestSource,&latestOffset,&latestTime)
 switch err {
 case nil:
  out.LatestRetainedSourceRef=currentSourceRef(latestSource)
  out.LatestRetainedMatchesSelected=latestSource==selectedCanonicalSource
  out.LatestRetainedOffset=&latestOffset
  t:=latestTime.UTC();out.LatestRetainedIngestedAt=&t
 case pgx.ErrNoRows:
  // Explicit absence, not evidence of no gameplay or no cheating.
 default:
  return CurrentSourceEvidenceSnapshot{},errors.New("latest retained source inspection failed")
 }
 rows,err:=tx.Query(ctx,`SELECT source_end_offset,ingested_at
 FROM case_evidence_events WHERE guild_id=$1 AND server_id=$2 AND source_id=$3
 ORDER BY id DESC LIMIT $4`,guildID,serverID,selectedCanonicalSource,limit+1)
 if err!=nil{return CurrentSourceEvidenceSnapshot{},errors.New("selected source inspection failed")}
 count:=0
 for rows.Next() {
  var offset int64
  var ingestedAt time.Time
  if err=rows.Scan(&offset,&ingestedAt);err!=nil{
   rows.Close();return CurrentSourceEvidenceSnapshot{},errors.New("invalid retained source address")
  }
  if count==0{
   out.LatestSelectedOffset=&offset
   t:=ingestedAt.UTC();out.LatestSelectedIngestedAt=&t
  }
  count++
 }
 err=rows.Err()
 rows.Close()
 if err!=nil{return CurrentSourceEvidenceSnapshot{},errors.New("retained source inspection failed")}
 out.SelectedSourceWindowTruncated=count>limit
 if count>limit{count=limit}
 out.SelectedSourceObservations=count
 if err=tx.Commit(ctx);err!=nil{return CurrentSourceEvidenceSnapshot{},errors.New("read-only source inspection commit failed")}
 return out,nil
}
