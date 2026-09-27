// case-shadow-qa-readback independently inspects one synthetic diagnostic.
// It contains no INSERT, UPDATE, DELETE, sanction or network endpoint.
package main

import (
 "context"
 "crypto/sha256"
 "errors"
 "fmt"
 "os"
 "strings"
 "time"

 "github.com/jackc/pgx/v5"
 "github.com/jackc/pgx/v5/pgxpool"
)

const (
 projectID = "0cb1c55e-71c6-4865-96a8-dc7c7e7b3dc9"
 serviceID = "98eb3f02-aca3-4d2e-bab9-5debb158d79f"
 databaseServiceID = "a12e5f66-542c-4689-b822-3072392bd2b0"
 sourceID = "CASE-QA-SYNTHETIC-ONLY"
 guildKey = "case-qa-synthetic-staging-only"
 providerKey = "case-qa-synthetic-only"
 fingerprint = "a72f75a559fca546f37fb84540eed89425c11aa57a39687b8985518938409696"
)
func authorize(project,service,database,dsn,expectedHost string)error{
 if project!=projectID||service!=serviceID||database!=databaseServiceID||dsn==""||expectedHost==""{
  return errors.New("C.A.S.E. QA scope mismatch")
 }
 cfg,err:=pgxpool.ParseConfig(dsn)
 if err!=nil||!strings.EqualFold(cfg.ConnConfig.Host,expectedHost)||
  !strings.HasSuffix(strings.ToLower(expectedHost),".railway.internal"){
  return errors.New("C.A.S.E. QA private database mismatch")
 }
 return nil
}
func inspect(ctx context.Context,pool *pgxpool.Pool)(int64,int64,error){
 tx,err:=pool.BeginTx(ctx,pgx.TxOptions{IsoLevel:pgx.RepeatableRead,AccessMode:pgx.ReadOnly})
 if err!=nil{return 0,0,errors.New("read-only transaction unavailable")}
 defer tx.Rollback(ctx)
 var guild,server int64
 err=tx.QueryRow(ctx,`SELECT g.id,s.id FROM guilds g JOIN game_servers s ON s.guild_id=g.id
 WHERE g.discord_guild_id=$1 AND s.provider='qa-fixture' AND s.provider_service_id=$2
 AND s.game='dayz' AND s.platform='PLAYSTATION'`,guildKey,providerKey).Scan(&guild,&server)
 if err!=nil{return 0,0,errors.New("synthetic QA scope absent")}
 var total,blocked int
 err=tx.QueryRow(ctx,`SELECT COUNT(*),COUNT(*) FILTER (WHERE id=1 AND detector_id='CASE-MOV-001'
 AND detector_version='0.1.0' AND fingerprint=$3 AND status='BLOCKED'
 AND cardinality(reason_codes)=4)
 FROM case_shadow_evaluations WHERE guild_id=$1 AND server_id=$2`,guild,server,fingerprint).Scan(&total,&blocked)
 if err!=nil||total!=1||blocked!=1{return 0,0,errors.New("evaluation identity or status mismatch")}
 var evidenceCount,linkedCount int
 err=tx.QueryRow(ctx,`SELECT COUNT(*) FROM case_evidence_events WHERE guild_id=$1 AND server_id=$2`,guild,server).Scan(&evidenceCount)
 if err!=nil||evidenceCount!=2{return 0,0,errors.New("synthetic evidence count mismatch")}
 err=tx.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluation_evidence l
 JOIN case_evidence_events ev ON ev.guild_id=l.guild_id AND ev.server_id=l.server_id AND ev.id=l.evidence_id
 WHERE l.guild_id=$1 AND l.server_id=$2 AND l.evaluation_id=1 AND ev.source_id=$3
 AND ev.event_type='PLAYER_HIT' AND ev.adm_clock='17:20:01'
 AND ((ev.source_end_offset=110 AND ev.line_sha256=$4)
 OR (ev.source_end_offset=220 AND ev.line_sha256=$5))`,
 guild,server,sourceID,
 fmt.Sprintf("%x",sha256.Sum256([]byte(fmt.Sprintf("%s:%d",sourceID,110)))),
 fmt.Sprintf("%x",sha256.Sum256([]byte(fmt.Sprintf("%s:%d",sourceID,220))))).Scan(&linkedCount)
 if err!=nil||linkedCount!=2{return 0,0,errors.New("source-linked evidence mismatch")}
 var allLinks int
 err=tx.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluation_evidence
 WHERE guild_id=$1 AND server_id=$2`,guild,server).Scan(&allLinks)
 if err!=nil||allLinks!=2{return 0,0,errors.New("unexpected evaluation links")}
 if err=tx.Commit(ctx);err!=nil{return 0,0,errors.New("readback transaction failed")}
 return guild,server,nil
}
func run(ctx context.Context,project,service,database,dsn,host string)error{
 if err:=authorize(project,service,database,dsn,host);err!=nil{return err}
 cfg,err:=pgxpool.ParseConfig(dsn)
 if err!=nil{return errors.New("invalid database configuration")}
 cfg.MinConns=0
 cfg.MaxConns=2
 pool,err:=pgxpool.NewWithConfig(ctx,cfg)
 if err!=nil{return errors.New("QA readback connection failed")}
 defer pool.Close()
 guild,server,err:=inspect(ctx,pool)
 if err!=nil{return err}
 fmt.Printf("CASE_QA_INDEPENDENT_READBACK=PASS GUILD=%d SERVER=%d EVALUATION_ID=1 STATUS=BLOCKED EVIDENCE=2 TOTAL_EVALUATIONS=1 ENFORCEMENT=DISABLED\\n",guild,server)
 return nil
}
func main(){
 ctx,cancel:=context.WithTimeout(context.Background(),30*time.Second);defer cancel()
 err:=run(ctx,os.Getenv("RAILWAY_PROJECT_ID"),os.Getenv("RAILWAY_SERVICE_ID"),
 os.Getenv("CASE_QA_DATABASE_SERVICE_ID"),os.Getenv("DATABASE_URL"),os.Getenv("CASE_QA_DATABASE_HOST"))
 if err!=nil{fmt.Fprintln(os.Stderr,"CASE_QA_INDEPENDENT_READBACK=FAIL");os.Exit(2)}
}
