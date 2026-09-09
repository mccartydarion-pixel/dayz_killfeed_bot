package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/factions"
)

type FactionPresentationRepository struct{ pool *pgxpool.Pool }

func NewFactionPresentationRepository(pool *pgxpool.Pool) *FactionPresentationRepository {
	return &FactionPresentationRepository{pool: pool}
}

func (r *FactionPresentationRepository) Load(ctx context.Context, guildID, seasonID, factionID int64) (*factions.InfoPresentation, error) {
	var p factions.InfoPresentation
	var longest *float64
	err := r.pool.QueryRow(ctx, `SELECT f.name,f.tag,COALESCE(owner.display_name,''),COUNT(DISTINCT fm.player_id),f.created_at,COALESCE(s.name,''),COUNT(k.id) FILTER(WHERE k.killer_faction_id=f.id),COUNT(k.id) FILTER(WHERE k.victim_faction_id=f.id),COUNT(k.id) FILTER(WHERE k.killer_faction_id=f.id AND k.victim_faction_id IS NOT NULL AND k.killer_faction_id<>k.victim_faction_id),COUNT(k.id) FILTER(WHERE k.killer_faction_id=f.id AND k.victim_faction_id=f.id),COUNT(k.id) FILTER(WHERE k.war_id IS NOT NULL AND k.killer_faction_id=f.id),MAX(k.distance) FILTER(WHERE k.killer_faction_id=f.id),COALESCE((SELECT SUM(pp.season_points) FROM player_points pp JOIN faction_members fm2 ON fm2.player_id=pp.player_id AND fm2.faction_id=f.id WHERE pp.guild_id=f.guild_id),0),COUNT(DISTINCT aw.id) FROM factions f LEFT JOIN players owner ON owner.id=f.owner_player_id LEFT JOIN faction_members fm ON fm.faction_id=f.id AND fm.active LEFT JOIN seasons s ON s.id=$2 LEFT JOIN kills k ON k.guild_id=f.guild_id AND k.season_id=$2 AND (k.killer_faction_id=f.id OR k.victim_faction_id=f.id) LEFT JOIN faction_wars aw ON aw.guild_id=f.guild_id AND aw.status='ACTIVE' AND (aw.faction_a_id=f.id OR aw.faction_b_id=f.id) WHERE f.guild_id=$1 AND f.id=$3 GROUP BY f.id,f.name,f.tag,owner.display_name,f.created_at,s.name`, guildID, seasonID, factionID).Scan(&p.Name, &p.Tag, &p.OwnerName, &p.MemberCount, &p.CreatedAt, &p.SeasonName, &p.SeasonKills, &p.SeasonDeaths, &p.EnemyFactionKills, &p.TeamKills, &p.WarKills, &longest, &p.ChampionPoints, &p.ActiveWars)
	if err != nil {
		return nil, fmt.Errorf("load faction presentation: %w", err)
	}
	p.LongestKillDistance = longest
	p.MaxMembers = 20
	if p.SeasonDeaths > 0 {
		p.SeasonKD = float64(p.SeasonKills) / float64(p.SeasonDeaths)
	} else {
		p.SeasonKD = float64(p.SeasonKills)
	}
	return &p, nil
}
