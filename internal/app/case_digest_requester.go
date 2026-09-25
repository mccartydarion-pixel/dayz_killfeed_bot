package app

import (
	"context"
	"errors"

	"github.com/yourname/dayz-killfeed/internal/permissions"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// caseWatchRequesterAllowed checks the SAME original website actor immediately
// before external delivery. Non-owner roles are reloaded via fresh Discord
// REST, bypassing the normal short-lived permission cache. No stale website
// session or role snapshot can authorize a queued staff message.
func (a *App) caseWatchRequesterAllowed(ctx context.Context,scope repository.AdminScope,userID int64)(bool,error){
	if a.caseWatchRequesterCheck!=nil{return a.caseWatchRequesterCheck(ctx,scope,userID)}
	if userID<=0 || a.DB==nil || a.DB.Pool==nil ||
		a.SaaSOrganizations==nil || a.Permissions==nil{return false,errors.New("staff authorization unavailable")}
	var discordID string
	err:=a.DB.Pool.QueryRow(ctx,"SELECT discord_user_id FROM app_users WHERE id=$1",userID).Scan(&discordID)
	if err!=nil{return false,err}
	org,err:=a.SaaSOrganizations.GetByID(ctx,scope.OrganizationID)
	if err!=nil{return false,err}
	if org==nil{return false,nil}
	if org.OwnerUserID==userID{return true,nil}
	if a.Discord==nil || a.Discord.Session()==nil{return false,errors.New("Discord role verification unavailable")}
	member,err:=a.Discord.Session().GuildMember(scope.DiscordGuildID,discordID)
	if err!=nil{return false,err}
	if member==nil{return false,nil}
	levelNames,err:=a.Permissions.LevelsForRoles(ctx,scope.InstallationID,member.Roles)
	if err!=nil{return false,err}
	for _,name:=range levelNames {
		if level,ok:=permissions.ParseLevel(name);ok && permissions.Allows(level,permissions.CapPlayerLocationView){
			return true,nil
		}
	}
	return false,nil
}
