//go:build integration

package repository

import (
 "context"
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
