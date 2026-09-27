// Command case-shadow-qa-setup initializes synthetic fixture evidence only in the
// dedicated Railway C.A.S.E. staging service. It must never run in the live bot.
package main

import (
 "context"
 "crypto/sha256"
 "errors"
 "fmt"
 "os"
 "strings"
 "time"

 "github.com/jackc/pgx/v5/pgxpool"
 "github.com/yourname/dayz-killfeed/internal/database"
)

const (
 qaProjectID = "0cb1c55e-71c6-4865-96a8-dc7c7e7b3dc9"
 qaServiceID = "98eb3f02-aca3-4d2e-bab9-5debb158d79f"
 qaGuildKey = "case-qa-synthetic-staging-only"
 qaProviderKey = "case-qa-synthetic-only"
 qaSource = "CASE-QA-SYNTHETIC-ONLY"
)

func authorize(project, service, gate, dsn, expectedHost string) error {
 if project != qaProjectID || service != qaServiceID || gate != "1" {
  return errors.New("dedicated C.A.S.E. staging gate required")
 }
 if dsn=="" {return errors.New("private QA database connection required")}
 cfg,err:=pgxpool.ParseConfig(dsn)
 if err!=nil{return errors.New("invalid QA database configuration")}
 host:=strings.ToLower(cfg.ConnConfig.Host)
 if expectedHost=="" || !strings.EqualFold(host,expectedHost) || !strings.HasSuffix(host,".railway.internal") {
  return errors.New("QA setup requires a private Railway database host")
 }
 return nil
}

func run(ctx context.Context,project,service,gate,dsn,expectedHost string) error {
 if err:=authorize(project,service,gate,dsn,expectedHost);err!=nil{return err}
 db,err:=database.Connect(ctx,dsn)
 if err!=nil{return errors.New("QA database unavailable")}
 defer db.Close()
 if err=db.Migrate(ctx);err!=nil{return errors.New("QA schema migration failed")}
 var guildID,serverID int64
 err=db.Pool.QueryRow(ctx,`INSERT INTO guilds(discord_guild_id)
 VALUES($1) ON CONFLICT(discord_guild_id)
 DO UPDATE SET discord_guild_id=EXCLUDED.discord_guild_id
 RETURNING id`,qaGuildKey).Scan(&guildID)
 if err!=nil{return errors.New("QA guild fixture failed")}
 err=db.Pool.QueryRow(ctx,`INSERT INTO game_servers
 (guild_id,provider,provider_service_id,game,platform,status,display_name)
 VALUES($1,'qa-fixture',$2,'dayz','PLAYSTATION','ACTIVE','CASE QA Synthetic')
 ON CONFLICT(guild_id,provider,provider_service_id)
 DO UPDATE SET display_name=EXCLUDED.display_name RETURNING id`,guildID,qaProviderKey).Scan(&serverID)
 if err!=nil{return errors.New("QA server fixture failed")}
 for _,offset:=range []int64{110,220}{
  hash:=sha256.Sum256([]byte(fmt.Sprintf("%s:%d",qaSource,offset)))
  tag,err:=db.Pool.Exec(ctx,`INSERT INTO case_evidence_events
  (guild_id,server_id,source_id,source_end_offset,line_sha256,event_type,adm_clock)
  VALUES($1,$2,$3,$4,$5,'PLAYER_HIT','17:20:01')
  ON CONFLICT (guild_id,server_id,source_id,source_end_offset) DO NOTHING`,
   guildID,serverID,qaSource,offset,fmt.Sprintf("%x",hash))
  if err!=nil{return errors.New("QA synthetic evidence insert failed")}
  _=tag
 }
 var evidenceCount,otherCount,evaluationCount int
 err=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FILTER(WHERE source_id=$3),
 COUNT(*) FILTER(WHERE source_id<>$3)
 FROM case_evidence_events WHERE guild_id=$1 AND server_id=$2`,
 guildID,serverID,qaSource).Scan(&evidenceCount,&otherCount)
 if err!=nil||evidenceCount!=2||otherCount!=0{return errors.New("QA evidence scope mismatch")}
 err=db.Pool.QueryRow(ctx,`SELECT COUNT(*) FROM case_shadow_evaluations WHERE guild_id=$1 AND server_id=$2`,guildID,serverID).Scan(&evaluationCount)
 if err!=nil||evaluationCount!=0{return errors.New("QA setup expected zero evaluations")}
 fmt.Printf("CASE_QA_SETUP=PASS GUILD=%d SERVER=%d SYNTHETIC_EVIDENCE=%d EVALUATIONS=%d ENFORCEMENT=DISABLED\n",
  guildID,serverID,evidenceCount,evaluationCount)
 return nil
}

func main(){
 ctx,cancel:=context.WithTimeout(context.Background(),90*time.Second);defer cancel()
 if err:=run(ctx,os.Getenv("RAILWAY_PROJECT_ID"),os.Getenv("RAILWAY_SERVICE_ID"),
  os.Getenv("CASE_QA_SETUP_ALLOWED"),os.Getenv("DATABASE_URL"),os.Getenv("CASE_QA_DATABASE_HOST"));err!=nil{
  fmt.Fprintln(os.Stderr,"CASE_QA_SETUP=FAILED (no credentials or source content logged)")
  os.Exit(2)
 }
}
