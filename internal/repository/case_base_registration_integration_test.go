//go:build integration

package repository

import (
 "context"
 "errors"
 "testing"
 "time"
)

// Registration is inert: these constraints protect stored drafts and grants,
// but neither an ADM clock nor a reviewed row establishes an intrusion.
func TestCaseBaseRegistrationScopeAndGrantConstraints(t *testing.T) {
 repo,fx,seedPlayer:=newZoneTestWorld(t)
 ctx:=context.Background()
 owner:=seedPlayer("Owner")
 guest:=seedPlayer("Guest")
 var baseID int64
 err:=repo.pool.QueryRow(ctx,`INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','North Base',100,200,50) RETURNING id`,
 fx.InstallationID,fx.GuildRowID,fx.ServerRowID,owner).Scan(&baseID)
 if err!=nil{t.Fatal(err)}
 var state string
 if err=repo.pool.QueryRow(ctx,`SELECT state FROM case_registered_bases
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND id=$4`,
 fx.InstallationID,fx.GuildRowID,fx.ServerRowID,baseID).Scan(&state);err!=nil||state!="DRAFT"{
  t.Fatalf("draft scope/readback: %q %v",state,err)
 }
 // A reviewed state must have an explicit review time; it still is not proof
 // of a DayZ event time, faction membership, or a detector violation.
 if _,err=repo.pool.Exec(ctx,`UPDATE case_registered_bases SET state='REVIEWED'
 WHERE id=$1`,baseID);err==nil{t.Fatal("reviewed without time accepted")}
 otherGuild:=int64(0)
 if err=repo.pool.QueryRow(ctx,`INSERT INTO guilds(discord_guild_id) VALUES($1) RETURNING id`,
  "case-base-other-guild").Scan(&otherGuild);err!=nil{t.Fatal(err)}
 var foreignPlayer int64
 if err=repo.pool.QueryRow(ctx,`INSERT INTO players(guild_id,dayz_player_id,display_name)
 VALUES($1,'case-base-foreign','Foreign') RETURNING id`,otherGuild).Scan(&foreignPlayer);err!=nil{t.Fatal(err)}
 if _,err=repo.pool.Exec(ctx,`INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Foreign',100,200,50)`,
 fx.InstallationID,fx.GuildRowID,fx.ServerRowID,foreignPlayer);err==nil{
  t.Fatal("foreign owner accepted")
 }
 var secondServer int64
 if err=repo.pool.QueryRow(ctx,`INSERT INTO game_servers
 (guild_id,provider,provider_service_id,game,platform,status)
 VALUES($1,'qa-fixture','case-base-other','dayz','PLAYSTATION','ACTIVE') RETURNING id`,
 fx.GuildRowID).Scan(&secondServer);err!=nil{t.Fatal(err)}
 if _,err=repo.pool.Exec(ctx,`INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 VALUES($1,$2,$3,$4,'chernarusplus','Wrong server',100,200,50)`,
 fx.InstallationID,fx.GuildRowID,secondServer,owner);err==nil{
  t.Fatal("base crossed installation server")
 }
 from:=time.Now().UTC()
 if _,err=repo.pool.Exec(ctx,`INSERT INTO case_base_authorizations
 (installation_id,guild_id,server_id,base_id,player_id,valid_from)
 VALUES($1,$2,$3,$4,$5,$6)`,
 fx.InstallationID,fx.GuildRowID,fx.ServerRowID,baseID,guest,from);err!=nil{t.Fatal(err)}
 for _,tc:=range []struct{name string;installation,guild,server,player int64;until *time.Time}{
  {"wrong server",fx.InstallationID,fx.GuildRowID,secondServer,guest,nil},
  {"foreign player",fx.InstallationID,fx.GuildRowID,fx.ServerRowID,foreignPlayer,nil},
  {"foreign guild",fx.InstallationID,otherGuild,fx.ServerRowID,foreignPlayer,nil},
 }{
  if _,err=repo.pool.Exec(ctx,`INSERT INTO case_base_authorizations
  (installation_id,guild_id,server_id,base_id,player_id,valid_from)
  VALUES($1,$2,$3,$4,$5,$6)`,
  tc.installation,tc.guild,tc.server,baseID,tc.player,from);err==nil{
   t.Fatalf("%s grant accepted",tc.name)
  }
 }
 if _,err=repo.pool.Exec(ctx,`INSERT INTO case_base_authorizations
 (installation_id,guild_id,server_id,base_id,player_id,valid_from,valid_until)
 VALUES($1,$2,$3,$4,$5,$6,$6)`,
 fx.InstallationID,fx.GuildRowID,fx.ServerRowID,baseID,guest,from);err==nil{
  t.Fatal("empty authorization interval accepted")
 }
 reader:=NewCaseBaseRegistrationRepository(repo.pool)
 bases,err:=reader.ListBases(ctx,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,0,10)
 if err!=nil||len(bases)!=1||bases[0].ID!=baseID||bases[0].OwnerPlayerID!=owner||
  bases[0].State!="DRAFT"||bases[0].MapKey!="chernarusplus"{
  t.Fatalf("scoped base read: %+v %v",bases,err)
 }
 grants,err:=reader.ListAuthorizations(ctx,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,baseID)
 if err!=nil||len(grants)!=1||grants[0].PlayerID==nil||*grants[0].PlayerID!=guest{
  t.Fatalf("scoped grant read: %+v %v",grants,err)
 }
 for _,scope:=range []struct{installation,guild,server int64}{
  {fx.InstallationID,fx.GuildRowID,secondServer},
  {fx.InstallationID,otherGuild,fx.ServerRowID},
  {fx.InstallationID+999,fx.GuildRowID,fx.ServerRowID},
 }{
  foreign,err:=reader.ListBases(ctx,scope.installation,scope.guild,scope.server,0,10)
  if err!=nil||len(foreign)!=0{t.Fatalf("foreign base read: %+v %v",foreign,err)}
  foreignGrants,err:=reader.ListAuthorizations(ctx,scope.installation,scope.guild,scope.server,baseID)
  if err!=nil||len(foreignGrants)!=0{t.Fatalf("foreign grants read: %+v %v",foreignGrants,err)}
 }
 if _,err=reader.ListBases(ctx,0,fx.GuildRowID,fx.ServerRowID,0,10);err==nil{t.Fatal("invalid scope accepted")}

}

func TestCaseBaseGrantWithdrawRaceCannotPersistGrant(t *testing.T) {
 repo,fx,seedPlayer:=newZoneTestWorld(t)
 ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
 defer cancel()
 owner:=seedPlayer("Race owner")
 guest:=seedPlayer("Race guest")
 reader:=NewCaseBaseRegistrationRepository(repo.pool)
 base,err:=reader.CreateDraft(ctx,CaseBaseDraftInput{
  InstallationID:fx.InstallationID,GuildID:fx.GuildRowID,ServerID:fx.ServerRowID,
  OwnerPlayerID:owner,MapKey:"chernarusplus",Name:"Race base",CenterX:10,CenterZ:20,Radius:30,
 })
 if err!=nil{t.Fatal(err)}
 tx,err:=repo.pool.Begin(ctx)
 if err!=nil{t.Fatal(err)}
 defer tx.Rollback(context.Background())
 var locked int64
 if err=tx.QueryRow(ctx,`SELECT id FROM case_registered_bases WHERE id=$1 FOR UPDATE`,base.ID).Scan(&locked);err!=nil{t.Fatal(err)}
 done:=make(chan error,1)
 go func(){
  _,grantErr:=reader.AddDraftGrant(ctx,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,&guest,nil,nil)
  done<-grantErr
 }()
 select{
 case grantErr:=<-done:
  t.Fatalf("grant did not wait for base claim lock: %v",grantErr)
 case <-time.After(40*time.Millisecond):
 }
 if _,err=tx.Exec(ctx,`UPDATE case_registered_bases SET state='REVOKED',revoked_at=NOW() WHERE id=$1`,base.ID);err!=nil{t.Fatal(err)}
 if err=tx.Commit(ctx);err!=nil{t.Fatal(err)}
 select{
 case grantErr:=<-done:
  if grantErr==nil{t.Fatal("grant committed after withdrawal")}
 case <-ctx.Done():
  t.Fatalf("grant did not settle after withdrawal: %v",ctx.Err())
 }
 var count int
 if err=repo.pool.QueryRow(ctx,`SELECT count(*) FROM case_base_authorizations WHERE base_id=$1`,base.ID).Scan(&count);err!=nil||count!=0{
  t.Fatalf("withdrawn draft retains %d new grants: %v",count,err)
 }
 // Direct writers must also obey the draft state guard; the API is not
 // the only path able to write to the table.
 _,err=repo.pool.Exec(ctx,`INSERT INTO case_base_authorizations
 (installation_id,guild_id,server_id,base_id,player_id,valid_from)
 VALUES($1,$2,$3,$4,$5,NOW())`,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,guest)
 if errors.Is(err,context.DeadlineExceeded)||err==nil{t.Fatalf("direct grant on withdrawn draft accepted or timed out: %v",err)}
}

func TestCaseBaseGrantIntervalsRejectOverlapAndAllowRenewal(t *testing.T) {
 repo,fx,seedPlayer:=newZoneTestWorld(t)
 ctx:=context.Background()
 owner:=seedPlayer("Interval owner")
 guest:=seedPlayer("Interval guest")
 second:=seedPlayer("Other guest")
 base,err:=NewCaseBaseRegistrationRepository(repo.pool).CreateDraft(ctx,CaseBaseDraftInput{
  InstallationID:fx.InstallationID,GuildID:fx.GuildRowID,ServerID:fx.ServerRowID,
  OwnerPlayerID:owner,MapKey:"chernarusplus",Name:"Interval base",CenterX:1,CenterZ:2,Radius:30,
 })
 if err!=nil{t.Fatal(err)}
 from:=time.Now().UTC().Add(-2*time.Hour)
 until:=from.Add(time.Hour)
 insert:=`INSERT INTO case_base_authorizations
 (installation_id,guild_id,server_id,base_id,player_id,valid_from,valid_until)
 VALUES($1,$2,$3,$4,$5,$6,$7)`
 args:=[]any{fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,guest,from,until}
 if _,err=repo.pool.Exec(ctx,insert,args...);err!=nil{t.Fatal(err)}
 if _,err=repo.pool.Exec(ctx,insert,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,guest,from.Add(30*time.Minute),until.Add(time.Hour));err==nil{
  t.Fatal("overlapping player grant accepted")
 }
 if _,err=repo.pool.Exec(ctx,insert,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,guest,until,until.Add(time.Hour));err!=nil{
  t.Fatalf("adjacent renewal rejected: %v",err)
 }
 if _,err=repo.pool.Exec(ctx,insert,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,second,from,until);err!=nil{
  t.Fatalf("different player grant rejected: %v",err)
 }
 if _,err=repo.pool.Exec(ctx,`UPDATE case_base_authorizations SET valid_until=$1
 WHERE base_id=$2 AND player_id=$3 AND valid_from=$4`,until.Add(30*time.Minute),base.ID,guest,from);err==nil{
  t.Fatal("grant extension into renewal interval accepted")
 }
 var factionID int64
 if err=repo.pool.QueryRow(ctx,`INSERT INTO factions(guild_id,name,tag,owner_player_id)
 VALUES($1,'Interval faction ' || $3::bigint::text,'I' || $3::bigint::text,$2) RETURNING id`,fx.GuildRowID,owner,base.ID).Scan(&factionID);err!=nil{t.Fatal(err)}
 factionInsert:=`INSERT INTO case_base_authorizations
 (installation_id,guild_id,server_id,base_id,faction_id,valid_from,valid_until)
 VALUES($1,$2,$3,$4,$5,$6,$7)`
 if _,err=repo.pool.Exec(ctx,factionInsert,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,factionID,from,until);err!=nil{
  t.Fatalf("faction grant rejected: %v",err)
 }
 if _,err=repo.pool.Exec(ctx,factionInsert,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,factionID,from.Add(time.Minute),until);err==nil{
  t.Fatal("overlapping faction grant accepted")
 }
 // A direct writer may close an interval, but cannot rewrite who it
 // authorized, when it began, or reopen/extend a closed grant.
 if _,err=repo.pool.Exec(ctx,`UPDATE case_base_authorizations SET player_id=$1
 WHERE base_id=$2 AND player_id=$3 AND valid_from=$4`,second,base.ID,guest,from);err==nil{
  t.Fatal("grant subject rewrite accepted")
 }
 if _,err=repo.pool.Exec(ctx,`UPDATE case_base_authorizations SET valid_from=$1
 WHERE base_id=$2 AND player_id=$3 AND valid_from=$4`,from.Add(-time.Minute),base.ID,second,from);err==nil{
  t.Fatal("grant start rewrite accepted")
 }
 if _,err=repo.pool.Exec(ctx,`UPDATE case_base_authorizations SET valid_until=$1
 WHERE base_id=$2 AND player_id=$3`,until.Add(time.Minute),base.ID,second);err==nil{
  t.Fatal("closed grant extension accepted")
 }
 if _,err=repo.pool.Exec(ctx,`UPDATE case_base_authorizations SET valid_until=NULL
 WHERE base_id=$1 AND player_id=$2`,base.ID,second);err==nil{
  t.Fatal("closed grant reopened")
 }
 shorter:=from.Add(30*time.Minute)
 if _,err=repo.pool.Exec(ctx,`UPDATE case_base_authorizations SET valid_until=$1
 WHERE base_id=$2 AND player_id=$3`,shorter,base.ID,second);err!=nil{
  t.Fatalf("closing grant early rejected: %v",err)
 }
 var observed time.Time
 if err=repo.pool.QueryRow(ctx,`SELECT valid_until FROM case_base_authorizations
 WHERE base_id=$1 AND player_id=$2`,base.ID,second).Scan(&observed);err!=nil||!observed.Equal(shorter.Truncate(time.Microsecond)){
  t.Fatalf("grant close readback=%v err=%v",observed,err)
 }
 var count int
 if err=repo.pool.QueryRow(ctx,`SELECT count(*) FROM case_base_authorizations WHERE base_id=$1`,base.ID).Scan(&count);err!=nil||count!=4{
  t.Fatalf("grant history count=%d err=%v",count,err)
 }
}

func TestCaseBaseGrantConcurrentOverlapSerializes(t *testing.T) {
 repo,fx,seedPlayer:=newZoneTestWorld(t)
 ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second)
 defer cancel()
 owner:=seedPlayer("Concurrent owner")
 guest:=seedPlayer("Concurrent guest")
 base,err:=NewCaseBaseRegistrationRepository(repo.pool).CreateDraft(ctx,CaseBaseDraftInput{
  InstallationID:fx.InstallationID,GuildID:fx.GuildRowID,ServerID:fx.ServerRowID,
  OwnerPlayerID:owner,MapKey:"chernarusplus",Name:"Concurrent base",CenterX:1,CenterZ:2,Radius:30,
 })
 if err!=nil{t.Fatal(err)}
 tx,err:=repo.pool.Begin(ctx)
 if err!=nil{t.Fatal(err)}
 defer tx.Rollback(context.Background())
 from:=time.Now().UTC()
 insert:=`INSERT INTO case_base_authorizations
 (installation_id,guild_id,server_id,base_id,player_id,valid_from)
 VALUES($1,$2,$3,$4,$5,$6)`
 if _,err=tx.Exec(ctx,insert,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,guest,from);err!=nil{
  t.Fatal(err)
 }
 done:=make(chan error,1)
 go func(){
  _,insertErr:=repo.pool.Exec(ctx,insert,fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,guest,from.Add(time.Minute))
  done<-insertErr
 }()
 select{
 case insertErr:=<-done:
  t.Fatalf("concurrent grant did not wait for claim lock: %v",insertErr)
 case <-time.After(40*time.Millisecond):
 }
 if err=tx.Commit(ctx);err!=nil{t.Fatal(err)}
 select{
 case insertErr:=<-done:
  if insertErr==nil{t.Fatal("overlapping grant committed after first transaction")}
 case <-ctx.Done():
  t.Fatalf("concurrent grant did not settle: %v",ctx.Err())
 }
 var count int
 if err=repo.pool.QueryRow(ctx,`SELECT count(*) FROM case_base_authorizations
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND base_id=$4 AND player_id=$5`,
  fx.InstallationID,fx.GuildRowID,fx.ServerRowID,base.ID,guest).Scan(&count);err!=nil||count!=1{
  t.Fatalf("concurrent grant history count=%d err=%v",count,err)
 }
}
