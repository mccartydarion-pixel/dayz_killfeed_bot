package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrCaseDigestCooldown = errors.New("case digest recently requested")
	ErrCaseDigestScope = errors.New("case digest installation or server scope mismatch")
	ErrCaseDigestClaimLost = errors.New("case digest claim no longer owned")
)

// CaseDigestOutbox is independent of free operational alerts and C.A.S.E.
// observation. It records a complete source-count snapshot, never player data.
type CaseDigestOutbox struct{ pool *pgxpool.Pool }

func NewCaseDigestOutbox(pool *pgxpool.Pool) *CaseDigestOutbox { return &CaseDigestOutbox{pool:pool} }

type CaseDigestInput struct {
	OrganizationID, InstallationID, GuildID, GameServerID, RequesterUserID int64
	WindowStart, WindowEnd time.Time
	SourceLines, HitLines, KillLines int64
	CollectorEnabled bool
}

type CaseDigestDelivery struct {
	ID, OrganizationID, InstallationID, GuildID, GameServerID, RequesterUserID int64
	WindowStart, WindowEnd time.Time
	SourceLines, HitLines, KillLines int64
	CollectorEnabled bool
	ClaimVersion int64
	Attempts int
}

type CaseDigestReceipt struct {
	ID, GameServerID int64
	Status string
	RequestedAt time.Time
	SentAt *time.Time
	DiscordChannelID, DiscordMessageID, ReasonCode *string
	Attempts int
}

// Enqueue takes an installation row-lock before checking its rolling-hour
// cooldown. Two app replicas cannot both insert a digest for that installation
// and server. The DB revalidates the full org/guild/server join under that
// lock; browser identifiers and a cached route are never authoritative.
func (r *CaseDigestOutbox) Enqueue(ctx context.Context, in CaseDigestInput) (int64,error) {
	if r==nil || r.pool==nil || in.OrganizationID<=0 || in.InstallationID<=0 ||
		in.GuildID<=0 || in.GameServerID<=0 || in.RequesterUserID<=0 || !in.WindowEnd.After(in.WindowStart) ||
		in.SourceLines<0 || in.HitLines<0 || in.KillLines<0 {
		return 0,ErrCaseDigestScope
	}
	tx,err:=r.pool.Begin(ctx)
	if err!=nil{return 0,err}
	defer tx.Rollback(ctx)
	var locked int64
	err=tx.QueryRow(ctx,`
		SELECT i.id FROM installations i
		JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id
		JOIN game_servers gs ON gs.id=i.game_server_id
		WHERE i.id=$1 AND i.organization_id=$2 AND i.game_server_id=$3
		  AND c.organization_id=$2 AND c.guild_id=$4
		  AND gs.guild_id=$4
		  AND (gs.organization_id IS NULL OR gs.organization_id=$2)
		FOR UPDATE OF i
	`,in.InstallationID,in.OrganizationID,in.GameServerID,in.GuildID).Scan(&locked)
	if errors.Is(err,pgx.ErrNoRows){return 0,ErrCaseDigestScope}
	if err!=nil{return 0,fmt.Errorf("lock case digest installation: %w",err)}
	var recent int64
	err=tx.QueryRow(ctx,`
		SELECT id FROM case_watch_digest_outbox
		WHERE organization_id=$1 AND installation_id=$2 AND game_server_id=$3
		  AND requested_at > NOW()-INTERVAL '1 hour'
		ORDER BY requested_at DESC,id DESC LIMIT 1
	`,in.OrganizationID,in.InstallationID,in.GameServerID).Scan(&recent)
	if err==nil{return 0,ErrCaseDigestCooldown}
	if !errors.Is(err,pgx.ErrNoRows){return 0,fmt.Errorf("check case digest cooldown: %w",err)}
	var id int64
	err=tx.QueryRow(ctx,`
		INSERT INTO case_watch_digest_outbox
		(organization_id,installation_id,guild_id,game_server_id,window_start,window_end,
		 source_lines,hit_lines,kill_lines,collector_enabled,requested_by_user_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id
	`,in.OrganizationID,in.InstallationID,in.GuildID,in.GameServerID,
		in.WindowStart,in.WindowEnd,in.SourceLines,in.HitLines,in.KillLines,in.CollectorEnabled,in.RequesterUserID).Scan(&id)
	if err!=nil{return 0,fmt.Errorf("insert case digest: %w",err)}
	if err=tx.Commit(ctx);err!=nil{return 0,fmt.Errorf("commit case digest: %w",err)}
	return id,nil
}

// ClaimNext leases only pre-send rows. A crashed CLAIMED lease can be
// retried; SENDING rows are never claimed again because Discord may have
// accepted the message immediately before a crash.
func (r *CaseDigestOutbox) ClaimNext(ctx context.Context) (*CaseDigestDelivery,error) {
	if r==nil || r.pool==nil{return nil,errors.New("case digest outbox unavailable")}
	var d CaseDigestDelivery
	err:=r.pool.QueryRow(ctx,`
		WITH candidate AS (
			SELECT id FROM case_watch_digest_outbox
			WHERE attempts<3 AND (
			  (status='READY' AND next_attempt_at<=NOW()) OR
			  (status='CLAIMED' AND claim_expires_at<NOW()))
			ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1
		)
		UPDATE case_watch_digest_outbox d SET
			status='CLAIMED',claim_version=d.claim_version+1,
			attempts=d.attempts+1,claim_expires_at=NOW()+INTERVAL '2 minutes',
			updated_at=NOW(),reason_code=NULL
		FROM candidate c WHERE d.id=c.id
		RETURNING d.id,d.organization_id,d.installation_id,d.guild_id,d.game_server_id,
		          d.window_start,d.window_end,d.source_lines,d.hit_lines,d.kill_lines,
		          d.collector_enabled,d.claim_version,d.attempts,COALESCE(d.requested_by_user_id,0)
	`).Scan(&d.ID,&d.OrganizationID,&d.InstallationID,&d.GuildID,&d.GameServerID,
		&d.WindowStart,&d.WindowEnd,&d.SourceLines,&d.HitLines,&d.KillLines,
		&d.CollectorEnabled,&d.ClaimVersion,&d.Attempts,&d.RequesterUserID)
	if errors.Is(err,pgx.ErrNoRows){return nil,nil}
	if err!=nil{return nil,fmt.Errorf("claim case digest: %w",err)}
	return &d,nil
}

// BeginSend is the irrevocable external-effect boundary. It compares the
// claim token and rechecks the exact current installation and destination
// mapping inside the SQL update. Once it commits, a missing receipt is
// UNKNOWN and must be reconciled, never automatically resent.
func (r *CaseDigestOutbox) BeginSend(ctx context.Context,d CaseDigestDelivery,channelID string) error {
	if channelID=="" {return ErrCaseDigestScope}
	tag,err:=r.pool.Exec(ctx,`
		UPDATE case_watch_digest_outbox d SET status='SENDING',
			discord_channel_id=$3,claim_expires_at=NULL,updated_at=NOW()
		WHERE d.id=$1 AND d.claim_version=$2 AND d.status='CLAIMED'
		  AND d.claim_expires_at>NOW()
		  AND EXISTS (
			SELECT 1 FROM installations i
			JOIN discord_guild_connections c ON c.id=i.discord_guild_connection_id
			JOIN game_servers gs ON gs.id=i.game_server_id
			JOIN installation_channel_routes cr ON cr.installation_id=i.id
			WHERE i.id=d.installation_id AND i.organization_id=d.organization_id
			  AND i.game_server_id=d.game_server_id
			  AND c.organization_id=d.organization_id AND c.guild_id=d.guild_id
			  AND gs.guild_id=d.guild_id
			  AND (gs.organization_id IS NULL OR gs.organization_id=d.organization_id)
			  AND cr.route_key='ADMIN_ALERTS' AND cr.channel_id=$3
		  )
	`,d.ID,d.ClaimVersion,channelID)
	if err!=nil{return err}
	if tag.RowsAffected()!=1{return ErrCaseDigestClaimLost}
	return nil
}

func (r *CaseDigestOutbox) MarkSent(ctx context.Context,d CaseDigestDelivery,messageID string) error {
	if messageID==""{return errors.New("missing Discord acknowledgement")}
	tag,err:=r.pool.Exec(ctx,`
		UPDATE case_watch_digest_outbox SET status='SENT',
		  discord_message_id=$3,sent_at=NOW(),updated_at=NOW(),reason_code=NULL
		WHERE id=$1 AND claim_version=$2 AND status='SENDING'
	`,d.ID,d.ClaimVersion,messageID)
	if err!=nil{return err}
	if tag.RowsAffected()!=1{return ErrCaseDigestClaimLost}
	return nil
}

// An API timeout / transport failure cannot prove Discord did not send;
// retain the one attempt for operator reconciliation, with no automatic retry.
func (r *CaseDigestOutbox) MarkUnknown(ctx context.Context,d CaseDigestDelivery,reason string) error {
	tag,err:=r.pool.Exec(ctx,`
		UPDATE case_watch_digest_outbox SET status='UNKNOWN',reason_code=$3,updated_at=NOW()
		WHERE id=$1 AND claim_version=$2 AND status='SENDING'
	`,d.ID,d.ClaimVersion,reason)
	if err!=nil{return err}
	if tag.RowsAffected()!=1{return ErrCaseDigestClaimLost}
	return nil
}

// Denied permission/payment/privacy/scope is terminal for that historical
// request; an operator can request a NEW digest only after the cooldown.
func (r *CaseDigestOutbox) BlockBeforeSend(ctx context.Context,d CaseDigestDelivery,reason string) error {
	tag,err:=r.pool.Exec(ctx,`
		UPDATE case_watch_digest_outbox SET status='BLOCKED',
		  reason_code=$3,claim_expires_at=NULL,updated_at=NOW()
		WHERE id=$1 AND claim_version=$2 AND status='CLAIMED'
	`,d.ID,d.ClaimVersion,reason)
	if err!=nil{return err}
	if tag.RowsAffected()!=1{return ErrCaseDigestClaimLost}
	return nil
}

// Only definitively pre-send transient errors retry (up to three claims).
func (r *CaseDigestOutbox) RetryBeforeSend(ctx context.Context,d CaseDigestDelivery,reason string) error {
	tag,err:=r.pool.Exec(ctx,`
		UPDATE case_watch_digest_outbox SET
		  status=CASE WHEN attempts>=3 THEN 'BLOCKED' ELSE 'READY' END,
		  reason_code=$3,claim_expires_at=NULL,
		  next_attempt_at=NOW()+attempts*INTERVAL '1 minute',updated_at=NOW()
		WHERE id=$1 AND claim_version=$2 AND status='CLAIMED'
	`,d.ID,d.ClaimVersion,reason)
	if err!=nil{return err}
	if tag.RowsAffected()!=1{return ErrCaseDigestClaimLost}
	return nil
}

// Sweep cannot replay an ambiguous external send. It only exposes the
// persisted uncertainty and closes exhausted pre-send leases.
func (r *CaseDigestOutbox) Sweep(ctx context.Context) error {
	_,err:=r.pool.Exec(ctx,`
		UPDATE case_watch_digest_outbox SET
			status=CASE WHEN status='SENDING' THEN 'UNKNOWN' ELSE 'BLOCKED' END,
			reason_code=CASE WHEN status='SENDING' THEN 'DELIVERY_UNCONFIRMED' ELSE 'PRE_SEND_RETRIES_EXHAUSTED' END,
			claim_expires_at=NULL,updated_at=NOW()
		WHERE (status='SENDING' AND updated_at<NOW()-INTERVAL '3 minutes')
		   OR (status='CLAIMED' AND attempts>=3 AND claim_expires_at<NOW())
	`)
	return err
}

func (r *CaseDigestOutbox) GetScoped(ctx context.Context,orgID,installationID,id int64) (*CaseDigestReceipt,error) {
	if r==nil||r.pool==nil{return nil,errors.New("case digest outbox unavailable")}
	var d CaseDigestReceipt
	err:=r.pool.QueryRow(ctx,`
		SELECT id,game_server_id,status,requested_at,sent_at,
		       discord_channel_id,discord_message_id,reason_code,attempts
		FROM case_watch_digest_outbox
		WHERE organization_id=$1 AND installation_id=$2 AND id=$3
	`,orgID,installationID,id).Scan(&d.ID,&d.GameServerID,&d.Status,&d.RequestedAt,&d.SentAt,
		&d.DiscordChannelID,&d.DiscordMessageID,&d.ReasonCode,&d.Attempts)
	if errors.Is(err,pgx.ErrNoRows){return nil,nil}
	if err!=nil{return nil,err}
	return &d,nil
}

// ReconcileUnknownWithVerifiedDiscordMessage is a receipt-only update after
// the caller has fetched the exact original channel/message via Discord and
// verified bot authorship + the embedded delivery reference. It never sends,
// never requeues and never grants a premium entitlement.
func (r *CaseDigestOutbox) ReconcileUnknownWithVerifiedDiscordMessage(ctx context.Context,
 orgID,installationID,serverID,id int64,channelID,messageID string) (bool,error){
 if r==nil||r.pool==nil{return false,errors.New("case digest outbox unavailable")}
 if orgID<=0||installationID<=0||serverID<=0||id<=0||channelID==""||messageID==""{
  return false,ErrCaseDigestScope
 }
 tag,err:=r.pool.Exec(ctx,`
  UPDATE case_watch_digest_outbox SET status='SENT',discord_message_id=$6,
   sent_at=NOW(),updated_at=NOW(),reason_code='MANUALLY_VERIFIED_DISCORD_MESSAGE'
  WHERE id=$1 AND organization_id=$2 AND installation_id=$3 AND game_server_id=$4
    AND status='UNKNOWN' AND discord_channel_id=$5 AND discord_message_id IS NULL
    AND EXISTS (SELECT 1 FROM installations i
                WHERE i.id=$3 AND i.organization_id=$2 AND i.game_server_id=$4)
 `,id,orgID,installationID,serverID,channelID,messageID)
 if err!=nil{return false,err}
 return tag.RowsAffected()==1,nil
}
