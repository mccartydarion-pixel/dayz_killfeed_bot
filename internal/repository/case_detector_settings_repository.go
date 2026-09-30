package repository

import (
 "context"
 "errors"
 "time"

 "github.com/jackc/pgx/v5/pgxpool"
 "github.com/yourname/dayz-killfeed/internal/caseintel"
)

// CaseDetectorSettingsRepository stores owner preferences only. No evaluator,
// case writer, Discord sender, or enforcement path consumes these rows.
type CaseDetectorSettingsRepository struct {pool *pgxpool.Pool}
func NewCaseDetectorSettingsRepository(pool *pgxpool.Pool)*CaseDetectorSettingsRepository{
 return &CaseDetectorSettingsRepository{pool:pool}
}

type CaseDetectorSetting struct {
 ModuleID string `json:"moduleId"`
 Sensitivity caseintel.Sensitivity `json:"sensitivity"`
 Configured bool `json:"configured"`
 Revision int64 `json:"revision"`
 UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

func validCaseModule(id string) bool {
 for _,def:=range caseintel.ClientCatalog(){if def.ID==id{return true}}
 return false
}
func validCaseSensitivity(mode caseintel.Sensitivity) bool {
 return mode==caseintel.SensitivityRelaxed||mode==caseintel.SensitivityBalanced||mode==caseintel.SensitivityStrict
}
func validCaseSettingsScope(installationID,guildID,serverID int64)bool{
 return installationID>0&&guildID>0&&serverID>0
}

func (r *CaseDetectorSettingsRepository) List(ctx context.Context,installationID,guildID,serverID int64)([]CaseDetectorSetting,error){
 if r==nil||r.pool==nil||!validCaseSettingsScope(installationID,guildID,serverID){
  return nil,errors.New("invalid C.A.S.E. settings scope")
 }
 rows,err:=r.pool.Query(ctx,`SELECT module_id,sensitivity,revision,updated_at
 FROM case_detector_settings
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3`,
  installationID,guildID,serverID)
 if err!=nil{return nil,err}
 defer rows.Close()
 overrides:=make(map[string]CaseDetectorSetting,8)
 for rows.Next(){
  var s CaseDetectorSetting
  var updated time.Time
  if err=rows.Scan(&s.ModuleID,&s.Sensitivity,&s.Revision,&updated);err!=nil{return nil,err}
  s.Configured=true;s.UpdatedAt=&updated
  overrides[s.ModuleID]=s
 }
 if err=rows.Err();err!=nil{return nil,err}
 out:=make([]CaseDetectorSetting,0,8)
 for _,def:=range caseintel.ClientCatalog(){
  if s,ok:=overrides[def.ID];ok{out=append(out,s)}else{
   out=append(out,CaseDetectorSetting{ModuleID:def.ID,Sensitivity:caseintel.SensitivityBalanced})
  }
 }
 return out,nil
}

func (r *CaseDetectorSettingsRepository) Set(ctx context.Context,installationID,guildID,serverID int64,moduleID string,mode caseintel.Sensitivity)(CaseDetectorSetting,error){
 if r==nil||r.pool==nil||!validCaseSettingsScope(installationID,guildID,serverID)||
  !validCaseModule(moduleID)||!validCaseSensitivity(mode){
  return CaseDetectorSetting{},errors.New("invalid C.A.S.E. detector preference")
 }
 s:=CaseDetectorSetting{ModuleID:moduleID,Configured:true}
 var updated time.Time
 err:=r.pool.QueryRow(ctx,`INSERT INTO case_detector_settings AS s
 (installation_id,guild_id,server_id,module_id,sensitivity)
 VALUES($1,$2,$3,$4,$5)
 ON CONFLICT (installation_id,guild_id,server_id,module_id) DO UPDATE
 SET sensitivity=EXCLUDED.sensitivity,
  revision=s.revision+CASE WHEN s.sensitivity IS DISTINCT FROM EXCLUDED.sensitivity THEN 1 ELSE 0 END,
  updated_at=CASE WHEN s.sensitivity IS DISTINCT FROM EXCLUDED.sensitivity THEN NOW() ELSE s.updated_at END
 RETURNING sensitivity,revision,updated_at`,
  installationID,guildID,serverID,moduleID,mode).Scan(&s.Sensitivity,&s.Revision,&updated)
 if err!=nil{return CaseDetectorSetting{},err}
 s.UpdatedAt=&updated
 return s,nil
}
