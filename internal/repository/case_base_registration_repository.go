package repository

import (
 "context"
 "errors"
 "math"
 "strings"
 "time"

 "github.com/jackc/pgx/v5"

 "github.com/jackc/pgx/v5/pgxpool"
)

// CaseBaseRegistrationRepository reads inert owner claims and time-bounded
// grants. Callers must derive this exact scope from an authenticated
// installation. No detector or notification path uses this repository.
type CaseBaseRegistrationRepository struct{ pool *pgxpool.Pool }

func NewCaseBaseRegistrationRepository(pool *pgxpool.Pool) *CaseBaseRegistrationRepository {
 return &CaseBaseRegistrationRepository{pool:pool}
}

type CaseRegisteredBase struct {
 ID int64 `json:"id"`
 InstallationID int64 `json:"installationId"`
 GuildID int64 `json:"guildId"`
 ServerID int64 `json:"serverId"`
 OwnerPlayerID int64 `json:"ownerPlayerId"`
 MapKey string `json:"mapKey"`
 Name string `json:"name"`
 State string `json:"state"`
 CenterX float64 `json:"centerX"`
 CenterZ float64 `json:"centerZ"`
 Radius float64 `json:"radius"`
 ReviewedAt *time.Time `json:"reviewedAt,omitempty"`
 RevokedAt *time.Time `json:"revokedAt,omitempty"`
}

type CaseBaseAuthorization struct {
 ID int64 `json:"id"`
 BaseID int64 `json:"baseId"`
 PlayerID *int64 `json:"playerId,omitempty"`
 FactionID *int64 `json:"factionId,omitempty"`
 ValidFrom time.Time `json:"validFrom"`
 ValidUntil *time.Time `json:"validUntil,omitempty"`
}

func (r *CaseBaseRegistrationRepository) ListBases(ctx context.Context, installationID,guildID,serverID,beforeID int64,limit int)([]CaseRegisteredBase,error){
 if r==nil||r.pool==nil||installationID<=0||guildID<=0||serverID<=0||beforeID<0 {
  return nil,errors.New("invalid C.A.S.E. base scope")
 }
 if limit<1||limit>100{limit=50}
 rows,err:=r.pool.Query(ctx,`SELECT id,installation_id,guild_id,server_id,owner_player_id,
 map_key,name,center_x,center_z,radius,state,reviewed_at,revoked_at
 FROM case_registered_bases
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3
 AND ($4::BIGINT=0 OR id<$4)
 ORDER BY id DESC LIMIT $5`,installationID,guildID,serverID,beforeID,limit)
 if err!=nil{return nil,err}
 defer rows.Close()
 out:=make([]CaseRegisteredBase,0)
 for rows.Next(){
  var b CaseRegisteredBase
  if err=rows.Scan(&b.ID,&b.InstallationID,&b.GuildID,&b.ServerID,&b.OwnerPlayerID,
   &b.MapKey,&b.Name,&b.CenterX,&b.CenterZ,&b.Radius,&b.State,&b.ReviewedAt,&b.RevokedAt);err!=nil{return nil,err}
  out=append(out,b)
 }
 return out,rows.Err()
}

func (r *CaseBaseRegistrationRepository) ListAuthorizations(ctx context.Context,installationID,guildID,serverID,baseID int64)([]CaseBaseAuthorization,error){
 if r==nil||r.pool==nil||installationID<=0||guildID<=0||serverID<=0||baseID<=0 {
  return nil,errors.New("invalid C.A.S.E. base scope")
 }
 rows,err:=r.pool.Query(ctx,`SELECT a.id,a.base_id,a.player_id,a.faction_id,a.valid_from,a.valid_until
 FROM case_base_authorizations a
 JOIN case_registered_bases b ON
  b.installation_id=a.installation_id AND b.guild_id=a.guild_id AND
  b.server_id=a.server_id AND b.id=a.base_id
 WHERE a.installation_id=$1 AND a.guild_id=$2 AND a.server_id=$3 AND a.base_id=$4
 ORDER BY a.id DESC LIMIT 101`,installationID,guildID,serverID,baseID)
 if err!=nil{return nil,err}
 defer rows.Close()
 out:=make([]CaseBaseAuthorization,0)
 for rows.Next(){
  var a CaseBaseAuthorization
  if err=rows.Scan(&a.ID,&a.BaseID,&a.PlayerID,&a.FactionID,&a.ValidFrom,&a.ValidUntil);err!=nil{return nil,err}
  out=append(out,a)
  if len(out)>100{return nil,errors.New("C.A.S.E. base authorization window exceeds safe bound")}
 }
 return out,rows.Err()
}

var ErrCaseBaseNotFound = errors.New("C.A.S.E. base draft not found")

type CaseBaseDraftInput struct {
 InstallationID, GuildID, ServerID, OwnerPlayerID int64
 MapKey, Name string
 CenterX, CenterZ, Radius float64
}

// CreateDraft records an owner-provided claim, not a verified base or detector
// authorization. The owner must be an existing player in the scoped guild.
func (r *CaseBaseRegistrationRepository) CreateDraft(ctx context.Context,in CaseBaseDraftInput)(CaseRegisteredBase,error){
 if r==nil||r.pool==nil||in.InstallationID<=0||in.GuildID<=0||in.ServerID<=0||in.OwnerPlayerID<=0||
  strings.TrimSpace(in.MapKey)!=in.MapKey||len(in.MapKey)==0||len(in.MapKey)>80||
  strings.TrimSpace(in.Name)!=in.Name||len(in.Name)==0||len(in.Name)>128||
  math.IsNaN(in.CenterX)||math.IsNaN(in.CenterZ)||math.IsNaN(in.Radius)||
  math.IsInf(in.CenterX,0)||math.IsInf(in.CenterZ,0)||math.IsInf(in.Radius,0)||
  in.CenterX< -100000||in.CenterX>100000||in.CenterZ< -100000||in.CenterZ>100000||
  in.Radius<1||in.Radius>5000 {
  return CaseRegisteredBase{},errors.New("invalid C.A.S.E. base draft")
 }
 var b CaseRegisteredBase
 err:=r.pool.QueryRow(ctx,`INSERT INTO case_registered_bases
 (installation_id,guild_id,server_id,owner_player_id,map_key,name,center_x,center_z,radius)
 SELECT $1,$2,$3,p.id,$5,$6,$7,$8,$9 FROM players p
 WHERE p.guild_id=$2 AND p.id=$4
 RETURNING id,installation_id,guild_id,server_id,owner_player_id,
 map_key,name,center_x,center_z,radius,state,reviewed_at,revoked_at`,
  in.InstallationID,in.GuildID,in.ServerID,in.OwnerPlayerID,in.MapKey,in.Name,in.CenterX,in.CenterZ,in.Radius).
  Scan(&b.ID,&b.InstallationID,&b.GuildID,&b.ServerID,&b.OwnerPlayerID,
   &b.MapKey,&b.Name,&b.CenterX,&b.CenterZ,&b.Radius,&b.State,&b.ReviewedAt,&b.RevokedAt)
 if errors.Is(err,pgx.ErrNoRows){return CaseRegisteredBase{},ErrCaseBaseNotFound}
 return b,err
}

// AddDraftGrant records a proposed player or faction authorization beginning
// at the server-side time. It cannot backdate a grant or alter a reviewed base.
func (r *CaseBaseRegistrationRepository) AddDraftGrant(ctx context.Context,
 installationID,guildID,serverID,baseID int64,playerID,factionID *int64,validUntil *time.Time)(CaseBaseAuthorization,error){
 if r==nil||r.pool==nil||installationID<=0||guildID<=0||serverID<=0||baseID<=0||
  (playerID==nil)==(factionID==nil)||(playerID!=nil&&*playerID<=0)||(factionID!=nil&&*factionID<=0) {
  return CaseBaseAuthorization{},errors.New("invalid C.A.S.E. base grant")
 }
 now:=time.Now().UTC()
 if validUntil!=nil && (!validUntil.After(now)||validUntil.After(now.AddDate(1,0,0))) {
  return CaseBaseAuthorization{},errors.New("invalid C.A.S.E. grant end time")
 }
 var a CaseBaseAuthorization
 err:=r.pool.QueryRow(ctx,`INSERT INTO case_base_authorizations
 (installation_id,guild_id,server_id,base_id,player_id,faction_id,valid_from,valid_until)
 SELECT b.installation_id,b.guild_id,b.server_id,b.id,$5,$6,$7,$8
 FROM case_registered_bases b
 WHERE b.installation_id=$1 AND b.guild_id=$2 AND b.server_id=$3 AND b.id=$4 AND b.state='DRAFT'
 AND (($5::BIGINT IS NOT NULL AND EXISTS(
   SELECT 1 FROM players p WHERE p.guild_id=$2 AND p.id=$5))
  OR ($6::BIGINT IS NOT NULL AND EXISTS(
   SELECT 1 FROM factions f WHERE f.guild_id=$2 AND f.id=$6 AND f.active)))
 RETURNING id,base_id,player_id,faction_id,valid_from,valid_until`,
 installationID,guildID,serverID,baseID,playerID,factionID,now,validUntil).
 Scan(&a.ID,&a.BaseID,&a.PlayerID,&a.FactionID,&a.ValidFrom,&a.ValidUntil)
 if errors.Is(err,pgx.ErrNoRows){return CaseBaseAuthorization{},ErrCaseBaseNotFound}
 return a,err
}
