package repository

import (
	"context"
	"time"
)

// SecuritySalesRow is one service's Security Store numbers on a server.
type SecuritySalesRow struct {
	ServiceID     string `json:"serviceId"`
	Sales         int    `json:"sales"`
	PointsEarned  int64  `json:"pointsEarned"`
	Buyers        int    `json:"buyers"`
	ActivePlayers int    `json:"activePlayers"`
	// Gifts is how many free gifts the owner gave in the period (not sales).
	Gifts int `json:"gifts"`
}

// SecuritySalesSummary is every sellable service's numbers for a period, plus
// the newest sales across all of them.
type SecuritySalesSummary struct {
	Since          time.Time          `json:"since"`
	Services       []SecuritySalesRow `json:"services"`
	TotalSales     int                `json:"totalSales"`
	TotalPoints    int64              `json:"totalPoints"`
	PlayersWithAny int                `json:"playersWithAny"`
	Recent         []SecurityPurchase `json:"recent"`
}

// SalesSummary counts sales and Champion Points per sellable service since
// the given time, how many players have paid time right now, and lists the
// newest sales. Every sellable service gets a row, even with no sales.
func (r *SecurityServiceRepository) SalesSummary(ctx context.Context, s SecurityScope, since time.Time, recentLimit int) (SecuritySalesSummary, error) {
	out := SecuritySalesSummary{Since: since, Services: make([]SecuritySalesRow, 0), Recent: make([]SecurityPurchase, 0)}
	if r == nil || r.pool == nil || !s.valid() || since.IsZero() {
		return out, ErrSecurityInvalidRequest
	}
	if recentLimit < 1 || recentLimit > 50 {
		recentLimit = 20
	}
	services := []string{ServiceBaseRaidAlarm, ServicePerimeterWatch, ServiceBaseBlackBox, ServiceFactionSecurity, ServiceSentinelPro}
	rows, err := r.pool.Query(ctx, `SELECT svc,
  COUNT(p.id) FILTER (WHERE p.created_at>=$5 AND p.ledger_entry_id IS NOT NULL),
  COALESCE(SUM(p.price_points) FILTER (WHERE p.created_at>=$5 AND p.ledger_entry_id IS NOT NULL),0)::BIGINT,
  COUNT(DISTINCT p.player_id) FILTER (WHERE p.created_at>=$5 AND p.ledger_entry_id IS NOT NULL),
  COUNT(DISTINCT p.player_id) FILTER (WHERE p.starts_at<=NOW() AND p.ends_at>NOW()),
  COUNT(p.id) FILTER (WHERE p.created_at>=$5 AND p.ledger_entry_id IS NULL)
 FROM unnest($4::TEXT[]) WITH ORDINALITY AS u(svc,ord)
 LEFT JOIN security_service_purchases p
  ON p.installation_id=$1 AND p.guild_id=$2 AND p.server_id=$3 AND p.service_id=u.svc
 GROUP BY svc,ord ORDER BY ord`, s.InstallationID, s.GuildID, s.ServerID, services, since)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var row SecuritySalesRow
		if err := rows.Scan(&row.ServiceID, &row.Sales, &row.PointsEarned, &row.Buyers, &row.ActivePlayers, &row.Gifts); err != nil {
			rows.Close()
			return out, err
		}
		out.TotalSales += row.Sales
		out.TotalPoints += row.PointsEarned
		out.Services = append(out.Services, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := r.pool.QueryRow(ctx, `SELECT COUNT(DISTINCT player_id) FROM security_service_purchases
 WHERE installation_id=$1 AND guild_id=$2 AND server_id=$3 AND starts_at<=NOW() AND ends_at>NOW()`,
		s.InstallationID, s.GuildID, s.ServerID).Scan(&out.PlayersWithAny); err != nil {
		return out, err
	}
	recent, err := r.pool.Query(ctx, `SELECT p.id,p.service_id,p.player_id,COALESCE(pl.display_name,''),p.price_points,p.duration_days,
  p.starts_at,p.ends_at,p.created_at,p.ledger_entry_id IS NULL
 FROM security_service_purchases p LEFT JOIN players pl ON pl.guild_id=p.guild_id AND pl.id=p.player_id
 WHERE p.installation_id=$1 AND p.guild_id=$2 AND p.server_id=$3
 ORDER BY p.created_at DESC,p.id DESC LIMIT $4`, s.InstallationID, s.GuildID, s.ServerID, recentLimit)
	if err != nil {
		return out, err
	}
	defer recent.Close()
	for recent.Next() {
		var p SecurityPurchase
		if err := recent.Scan(&p.ID, &p.ServiceID, &p.PlayerID, &p.PlayerName, &p.PricePoints, &p.DurationDays, &p.StartsAt, &p.EndsAt, &p.CreatedAt, &p.Gift); err != nil {
			return out, err
		}
		out.Recent = append(out.Recent, p)
	}
	return out, recent.Err()
}
