// Command case-shadow-once is an OFFLINE operator utility, never called by
// the bot, HTTP service or a scheduled job. Preview performs no writes.
// Execute requires a fresh matching fingerprint and an explicit environment
// acknowledgment; only BLOCKED prerequisite diagnostics can be persisted.
package main

import (
 "context"
 "errors"
 "flag"
 "fmt"
 "os"
 "slices"
 "strings"
 "time"

 "github.com/jackc/pgx/v5/pgxpool"
 "github.com/yourname/dayz-killfeed/internal/caseintel"
 "github.com/yourname/dayz-killfeed/internal/repository"
)

const detectorID="CASE-MOV-001"
const detectorVersion="0.1.0"
const executeAcknowledgment="BLOCKED_DIAGNOSTICS_ONLY"
const hardMax=50

type options struct {
 mode string
 guild,server int64
 limit int
 expected string
 ack string
}
type snapshot struct{
 ids []int64
 fingerprint string
 sourceRef string // only SHA-256 pseudonym of selected source, no raw path in output
}

func validate(o options, gate string)error{
 if o.mode!="preview" && o.mode!="execute"{return errors.New("mode must be preview or execute")}
 if o.guild<=0||o.server<=0||o.limit<1||o.limit>hardMax{return errors.New("positive guild/server IDs and limit 1-50 required")}
 if o.mode=="execute"{
  if gate!="1"{return errors.New("one-shot write gate is disabled")}
  if o.ack!=executeAcknowledgment{return errors.New("explicit blocked-only acknowledgment required")}
  if len(o.expected)!=64||strings.Trim(o.expected,"0123456789abcdef")!=""{return errors.New("64-character lowercase preflight fingerprint required")}
 }
 return nil
}

func selectSnapshot(ctx context.Context,pool *pgxpool.Pool,o options)(snapshot,error){
 // No cross-source stitching: source is chosen by the newest ingested row.
 // These offsets are source addresses, not elapsed gameplay time.
 tx,err:=pool.Begin(ctx);if err!=nil{return snapshot{},err};defer tx.Rollback(ctx)
 var source string
 err=tx.QueryRow(ctx,`SELECT source_id FROM case_evidence_events
  WHERE guild_id=$1 AND server_id=$2 ORDER BY id DESC LIMIT 1`,o.guild,o.server).Scan(&source)
 if err!=nil{return snapshot{},errors.New("no selected-source evidence for requested server")}
 rows,err:=tx.Query(ctx,`SELECT id FROM case_evidence_events
  WHERE guild_id=$1 AND server_id=$2 AND source_id=$3
  ORDER BY source_end_offset DESC,id DESC LIMIT $4`,o.guild,o.server,source,o.limit)
 if err!=nil{return snapshot{},errors.New("bounded evidence query failed")}
 ids:=make([]int64,0,o.limit)
 for rows.Next(){var id int64;if err=rows.Scan(&id);err!=nil{rows.Close();return snapshot{},err};ids=append(ids,id)}
 err=rows.Err();rows.Close();if err!=nil{return snapshot{},err}
 if len(ids)==0{return snapshot{},errors.New("no evidence in selected source")}
 fp,err:=caseintel.EvidenceFingerprint(o.guild,o.server,detectorID,detectorVersion,ids)
 if err!=nil{return snapshot{},err}
 // Deliberately do not emit physical source path or player identifiers.
 return snapshot{ids:ids,fingerprint:fp},nil
}

func main(){
 o:=options{}
 flag.StringVar(&o.mode,"mode","preview","preview is read-only; execute requires explicit gates")
 flag.Int64Var(&o.guild,"guild",0,"database guild ID")
 flag.Int64Var(&o.server,"server",0,"database game server ID")
 flag.IntVar(&o.limit,"limit",10,"bounded evidence window, 1-50")
 flag.StringVar(&o.expected,"expected-fingerprint","","exact fingerprint from preview (execute only)")
 flag.StringVar(&o.ack,"ack","","must equal BLOCKED_DIAGNOSTICS_ONLY (execute only)")
 flag.Parse()
 if err:=run(context.Background(),o,os.Getenv("CASE_SHADOW_ONESHOT_ALLOWED"),os.Getenv("DATABASE_PUBLIC_URL"),os.Getenv("DATABASE_URL"));err!=nil{
  // Do not print wrapped PG errors, DSNs, source paths or event content.
  fmt.Fprintln(os.Stderr,"CASE one-shot failed:",safeError(err))
  os.Exit(2)
 }
}

func safeError(err error)string{
 switch {
 case errors.Is(err,repository.ErrShadowDisabled):return "write gate disabled"
 default:return "preflight, evidence, or database operation failed; no player verdict produced"
 }
}

func run(parent context.Context,o options,gate,publicURL,privateURL string)error{
 if err:=validate(o,gate);err!=nil{return err}
 dsn:=publicURL;if dsn==""{dsn=privateURL}
 if dsn==""{return errors.New("database URL missing")}
 ctx,cancel:=context.WithTimeout(parent,25*time.Second);defer cancel()
 cfg,err:=pgxpool.ParseConfig(dsn);if err!=nil{return errors.New("invalid database configuration")}
 cfg.MaxConns=1;cfg.MinConns=0
 cfg.ConnConfig.RuntimeParams["application_name"]="champion-case-shadow-once"
 pool,err:=pgxpool.NewWithConfig(ctx,cfg);if err!=nil{return errors.New("database connection failed")}
 defer pool.Close()
 if err=pool.Ping(ctx);err!=nil{return errors.New("database unavailable")}
 snap,err:=selectSnapshot(ctx,pool,o);if err!=nil{return err}
 fmt.Printf("MODE=%s GUILD=%d SERVER=%d DETECTOR=%s VERSION=%s EVIDENCE_COUNT=%d FINGERPRINT=%s\n",
  o.mode,o.guild,o.server,detectorID,detectorVersion,len(snap.ids),snap.fingerprint)
 if o.mode=="preview"{fmt.Println("READ_ONLY: no evaluation recorded");return nil}
 if !strings.EqualFold(snap.fingerprint,o.expected){return errors.New("evidence window changed: fingerprint mismatch")}
 // Recheck the full snapshot immediately before the write. RecordBlocked
 // independently validates every evidence ID and guild/server inside its
 // insert transaction; neither stage can emit a finding or a sanction.
 snap2,err:=selectSnapshot(ctx,pool,o);if err!=nil{return err}
 if !slices.Equal(snap.ids,snap2.ids){return errors.New("evidence window changed during preflight")}
 ledger:=repository.NewShadowLedger(pool)
 id,err:=ledger.RecordBlocked(ctx,repository.BlockedShadowInput{
  Enabled:true,GuildID:o.guild,ServerID:o.server,
  DetectorID:detectorID,Version:detectorVersion,EvidenceIDs:snap.ids,
 })
 if err!=nil{return err}
 history,err:=ledger.ListShadowHistory(ctx,o.guild,o.server,nil,50)
 if err!=nil{return errors.New("post-write history read failed")}
 ok:=false
 for _,item:=range history {
  if item.ID!=id{continue}
  sorted:=append([]int64(nil),snap.ids...);slices.Sort(sorted)
  if item.Status!="BLOCKED"||!slices.Equal(sorted,item.EvidenceIDs)||len(item.ReasonCodes)!=4 {
   return errors.New("post-write history does not match expected blocked evaluation")
  }
  ok=true;break
 }
 if !ok{return errors.New("post-write readback not found in bounded history")}
 fmt.Printf("BLOCKED_EVALUATION_ID=%d EVIDENCE_COUNT=%d READBACK=PASS ENFORCEMENT=DISABLED\n",id,len(snap.ids))
 return nil
}
