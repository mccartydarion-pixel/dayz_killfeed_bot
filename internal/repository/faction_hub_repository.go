package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
)

// FactionHubRepository persists the Faction Hub (hub_factions, hub_faction_members,
// hub_faction_applications, hub_faction_settings; docs/FACTIONS.md). It is separate from
// FactionRepository, the Discord-side faction system.
//
// Every method takes the organization id AND the installation id, and every query is
// scoped by both (plus the faction id where one applies), so an id from another
// tenant answers ErrNotFound exactly like an id that does not exist. The schema backs
// this with composite foreign keys (see migration 0032_faction_hub).
//
// The rules that must not be raced - one active faction per user per installation,
// accepting an application, role changes - run in one transaction each and take the
// user-level advisory lock (lockHubUser) before any row lock, so two leaders accepting
// the same applicant serialize instead of deadlocking. Authorization (the acting user's
// faction role) is read inside that same transaction, so a role change cannot slip
// between the check and the write. Errors are the typed factionhub errors, never raw SQL.
type FactionHubRepository struct{ pool *pgxpool.Pool }

func NewFactionHubRepository(pool *pgxpool.Pool) *FactionHubRepository {
	return &FactionHubRepository{pool: pool}
}

// hubDB is what both the pool and a transaction provide.
type hubDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// HubFaction is a faction's public profile row.
type HubFaction struct {
	ID, OrganizationID, InstallationID, GameServerID int64
	Name, Tag, Slug, Description                     string
	RecruitmentStatus                                string
	// Visual keys (placeholders for the approved catalogs; never URLs) and colors.
	LogoKey, FlagKey, ArmbandKey, PrimaryColor, SecondaryColor *string
	CreatedByUserID                                            int64
	CreatedAt, UpdatedAt                                       time.Time
	MemberCount                                                int
	// Logo is the current logo asset (nil = the Champion default logo).
	Logo *factionhub.Asset
	// Settings is filled by single-faction reads (not by the directory).
	Settings factionhub.Settings
}

// HubUser is the public identity shown next to a member or applicant.
type HubUser struct {
	ID                                          int64
	DiscordUserID, Username, GlobalName, Avatar string
}

// HubMember is one faction membership.
type HubMember struct {
	ID, FactionID int64
	User          HubUser
	PlayerID      *int64
	// Gamertag is the linked DayZ player's display name, when the user has a verified link.
	Gamertag *string
	RoleKey  string
	JoinedAt time.Time
}

// HubApplication is one application. Faction* are filled by MyFaction only.
type HubApplication struct {
	ID, FactionID                        int64
	User                                 HubUser
	Status, Message                      string
	ReviewedByUserID                     *int64
	ReviewedAt                           *time.Time
	CreatedAt, UpdatedAt                 time.Time
	FactionName, FactionTag, FactionSlug string
}

// HubFactionInput is validated, normalized input for creating a faction.
type HubFactionInput struct {
	Name, Tag, Description, RecruitmentStatus string
}

// HubFactionUpdate is validated, normalized input for updating a faction. A nil field
// leaves the stored value unchanged; an empty color/flag/armband clears it; a non-nil Settings
// replaces the requirements as a whole.
type HubFactionUpdate struct {
	Name, Tag, Description, RecruitmentStatus *string
	PrimaryColor, SecondaryColor              *string
	// FlagKey / ArmbandKey are validated catalog keys; "" clears.
	FlagKey, ArmbandKey *string
	Settings            *factionhub.Settings
}

// HubAccess identifies the acting user. OrgViewer is true when the user is an
// OWNER/ADMIN of the organization: a read-only moderation view, never a mutation right.
type HubAccess struct {
	UserID    int64
	OrgViewer bool
}

// HubDirectoryQuery filters the directory. Results are newest-first (id DESC) keyset
// pages: AfterID is the last id of the previous page (0 = first page).
type HubDirectoryQuery struct {
	RecruitingOnly bool
	Search         string
	Limit          int
	AfterID        int64
}

// HubMyFaction is the acting user's standing on one installation.
type HubMyFaction struct {
	Faction *HubFaction // nil when the user is in no faction
	Member  *HubMember
	// Pending are the user's pending applications on this installation.
	Pending []HubApplication
}

// --- shared helpers ---------------------------------------------------------------------

const hubFactionCols = `f.id, f.organization_id, f.installation_id, f.game_server_id, f.name, f.tag, f.slug, f.description, f.recruitment_status,
 f.logo_key, f.flag_key, f.armband_key, f.primary_color, f.secondary_color, f.created_by_user_id, f.created_at, f.updated_at,
 (SELECT COUNT(*) FROM hub_faction_members m WHERE m.faction_id = f.id),
 la.id, la.public_id::text, la.storage_key, la.content_type, la.size_bytes, la.width, la.height, la.original_filename, la.created_at`

// hubFactionFrom joins the current logo asset (a faction with no logo has NULLs there).
const hubFactionFrom = `hub_factions f LEFT JOIN hub_faction_assets la ON la.id = f.logo_asset_id`

func scanHubFaction(row interface{ Scan(...any) error }) (HubFaction, error) {
	var f HubFaction
	var la struct {
		id                                      *int64
		publicID, storageKey, contentType, name *string
		size, width, height                     *int
		created                                 *time.Time
	}
	err := row.Scan(&f.ID, &f.OrganizationID, &f.InstallationID, &f.GameServerID, &f.Name, &f.Tag, &f.Slug, &f.Description, &f.RecruitmentStatus,
		&f.LogoKey, &f.FlagKey, &f.ArmbandKey, &f.PrimaryColor, &f.SecondaryColor, &f.CreatedByUserID, &f.CreatedAt, &f.UpdatedAt, &f.MemberCount,
		&la.id, &la.publicID, &la.storageKey, &la.contentType, &la.size, &la.width, &la.height, &la.name, &la.created)
	if err == nil && la.id != nil {
		f.Logo = &factionhub.Asset{ID: *la.id, PublicID: *la.publicID, FactionID: f.ID, StorageKey: *la.storageKey, ContentType: *la.contentType,
			SizeBytes: *la.size, Width: *la.width, Height: *la.height, OriginalFilename: *la.name, CreatedAt: *la.created}
	}
	return f, err
}

const hubMemberCols = `m.id, m.faction_id, u.id, u.discord_user_id, u.discord_username, COALESCE(u.discord_global_name,''), COALESCE(u.avatar,''),
 m.player_id, p.display_name, m.role_key, m.joined_at`

const hubMemberFrom = `FROM hub_faction_members m
JOIN app_users u ON u.id = m.user_id
LEFT JOIN players p ON p.id = m.player_id`

func scanHubMember(row interface{ Scan(...any) error }) (HubMember, error) {
	var m HubMember
	err := row.Scan(&m.ID, &m.FactionID, &m.User.ID, &m.User.DiscordUserID, &m.User.Username, &m.User.GlobalName, &m.User.Avatar,
		&m.PlayerID, &m.Gamertag, &m.RoleKey, &m.JoinedAt)
	return m, err
}

const hubApplicationCols = `a.id, a.faction_id, u.id, u.discord_user_id, u.discord_username, COALESCE(u.discord_global_name,''), COALESCE(u.avatar,''),
 a.status, a.message, a.reviewed_by_user_id, a.reviewed_at, a.created_at, a.updated_at, f.name, f.tag, f.slug`

const hubApplicationFrom = `FROM hub_faction_applications a
JOIN app_users u ON u.id = a.user_id
JOIN hub_factions f ON f.id = a.faction_id`

func scanHubApplication(row interface{ Scan(...any) error }) (HubApplication, error) {
	var a HubApplication
	err := row.Scan(&a.ID, &a.FactionID, &a.User.ID, &a.User.DiscordUserID, &a.User.Username, &a.User.GlobalName, &a.User.Avatar,
		&a.Status, &a.Message, &a.ReviewedByUserID, &a.ReviewedAt, &a.CreatedAt, &a.UpdatedAt, &a.FactionName, &a.FactionTag, &a.FactionSlug)
	return a, err
}

func hubConstraint(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// mapHubUnique turns the unique violations the Hub schema can raise into typed errors.
func mapHubUnique(err error) error {
	switch c, ok := hubConstraint(err); {
	case !ok:
		return err
	case c == "uq_hub_factions_installation_name":
		return factionhub.ErrNameTaken
	case c == "uq_hub_factions_installation_tag":
		return factionhub.ErrTagTaken
	case c == "uq_hub_members_installation_user", c == "uq_hub_members_faction_user":
		return factionhub.ErrAlreadyInFaction
	case c == "uq_hub_applications_pending":
		return factionhub.ErrAlreadyApplied
	}
	return err
}

// hubInstallation checks the installation belongs to the organization and reports what a
// write needs to know about it.
func hubInstallation(ctx context.Context, q hubDB, organizationID, installationID int64) (gameServerID *int64, status string, err error) {
	err = q.QueryRow(ctx, `SELECT game_server_id, status FROM installations WHERE id=$1 AND organization_id=$2`, installationID, organizationID).Scan(&gameServerID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", factionhub.ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("hub installation lookup: %w", err)
	}
	return gameServerID, status, nil
}

// hubWritableInstallation is hubInstallation plus the rules for creating/applying: a DayZ
// server must be selected (a faction belongs to a server context) and the installation
// must not be suspended.
func hubWritableInstallation(ctx context.Context, q hubDB, organizationID, installationID int64) (int64, error) {
	gameServerID, status, err := hubInstallation(ctx, q, organizationID, installationID)
	if err != nil {
		return 0, err
	}
	if status == InstallationSuspended {
		return 0, factionhub.ErrInstallationInert
	}
	if gameServerID == nil {
		return 0, factionhub.ErrNoServer
	}
	return *gameServerID, nil
}

// lockHubUser takes a transaction-scoped advisory lock for (installation, user). Every
// transaction that creates or changes a user's membership or applications on an
// installation takes it FIRST, before any row lock, which serializes them and rules out
// the lock-order deadlock between two accepts of the same applicant.
func lockHubUser(ctx context.Context, tx pgx.Tx, installationID, userID int64) error {
	key := fmt.Sprintf("hub_faction_user:%d:%d", installationID, userID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, key); err != nil {
		return fmt.Errorf("hub user lock: %w", err)
	}
	return nil
}

// hubFactionScoped selects the faction within (organization, installation), optionally
// locking the faction row ("", "FOR SHARE OF f" or "FOR UPDATE OF f").
func hubFactionScoped(ctx context.Context, q hubDB, organizationID, installationID, factionID int64, lock string) (HubFaction, error) {
	row := q.QueryRow(ctx, `SELECT `+hubFactionCols+` FROM `+hubFactionFrom+` WHERE f.id=$1 AND f.organization_id=$2 AND f.installation_id=$3 `+lock, factionID, organizationID, installationID)
	f, err := scanHubFaction(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return HubFaction{}, factionhub.ErrNotFound
	}
	if err != nil {
		return HubFaction{}, fmt.Errorf("hub faction lookup: %w", err)
	}
	return f, nil
}

func hubSettings(ctx context.Context, q hubDB, factionID int64) (factionhub.Settings, error) {
	var s factionhub.Settings
	err := q.QueryRow(ctx, `SELECT minimum_hours, minimum_age, pvp_required, builder_needed, mic_required, custom_requirements FROM hub_faction_settings WHERE faction_id=$1`, factionID).
		Scan(&s.MinimumHours, &s.MinimumAge, &s.PvPRequired, &s.BuilderNeeded, &s.MicRequired, &s.CustomRequirements)
	if errors.Is(err, pgx.ErrNoRows) {
		return factionhub.Settings{}, nil
	}
	if err != nil {
		return factionhub.Settings{}, fmt.Errorf("hub settings lookup: %w", err)
	}
	return s, nil
}

// hubFactionFull is hubFactionScoped plus the requirements/settings.
func hubFactionFull(ctx context.Context, q hubDB, organizationID, installationID, factionID int64) (HubFaction, error) {
	f, err := hubFactionScoped(ctx, q, organizationID, installationID, factionID, "")
	if err != nil {
		return HubFaction{}, err
	}
	f.Settings, err = hubSettings(ctx, q, factionID)
	return f, err
}

// hubRole returns the user's primary role in the faction, or "" when not a member.
func hubRole(ctx context.Context, q hubDB, factionID, userID int64) (string, error) {
	var role string
	err := q.QueryRow(ctx, `SELECT role_key FROM hub_faction_members WHERE faction_id=$1 AND user_id=$2`, factionID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("hub role lookup: %w", err)
	}
	return role, nil
}

// hubInstallationMembership reports whether the user is already in ANY faction on the installation.
func hubInstallationMembership(ctx context.Context, q hubDB, installationID, userID int64) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM hub_faction_members WHERE installation_id=$1 AND user_id=$2)`, installationID, userID).Scan(&exists); err != nil {
		return false, fmt.Errorf("hub membership lookup: %w", err)
	}
	return exists, nil
}

// hubAddMember inserts a membership, linking the user's verified DayZ player when there is one.
func hubAddMember(ctx context.Context, q hubDB, factionID, installationID, userID int64, role string) (int64, error) {
	// player_links.status 'VERIFIED' is linking.StatusVerified (not imported to keep the
	// repository free of the linking package).
	var playerID *int64
	err := q.QueryRow(ctx, `
SELECT pl.player_id
FROM installations i
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN player_links pl ON pl.guild_id = c.guild_id AND pl.status = 'VERIFIED'
JOIN app_users u ON u.discord_user_id = pl.discord_user_id
WHERE i.id=$1 AND u.id=$2`, installationID, userID).Scan(&playerID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("hub linked player lookup: %w", err)
	}
	var id int64
	err = q.QueryRow(ctx, `INSERT INTO hub_faction_members(faction_id, installation_id, user_id, player_id, role_key) VALUES($1,$2,$3,$4,$5) RETURNING id`,
		factionID, installationID, userID, playerID, role).Scan(&id)
	if err != nil {
		return 0, mapHubUnique(fmt.Errorf("hub add member: %w", err))
	}
	return id, nil
}

// hubCancelPending closes the user's other pending applications on the installation
// (they joined or founded a faction). exceptApplicationID may be 0.
func hubCancelPending(ctx context.Context, q hubDB, installationID, userID, exceptApplicationID int64) error {
	_, err := q.Exec(ctx, `UPDATE hub_faction_applications SET status='CANCELLED', updated_at=NOW() WHERE installation_id=$1 AND user_id=$2 AND status='PENDING' AND id<>$3`, installationID, userID, exceptApplicationID)
	if err != nil {
		return fmt.Errorf("hub cancel pending applications: %w", err)
	}
	return nil
}

func hubMemberByID(ctx context.Context, q hubDB, factionID, memberID int64, lock string) (HubMember, error) {
	m, err := scanHubMember(q.QueryRow(ctx, `SELECT `+hubMemberCols+` `+hubMemberFrom+` WHERE m.id=$1 AND m.faction_id=$2 `+lock, memberID, factionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return HubMember{}, factionhub.ErrNotFound
	}
	if err != nil {
		return HubMember{}, fmt.Errorf("hub member lookup: %w", err)
	}
	return m, nil
}

func hubApplicationByID(ctx context.Context, q hubDB, factionID, applicationID int64) (HubApplication, error) {
	a, err := scanHubApplication(q.QueryRow(ctx, `SELECT `+hubApplicationCols+` `+hubApplicationFrom+` WHERE a.id=$1 AND a.faction_id=$2`, applicationID, factionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return HubApplication{}, factionhub.ErrNotFound
	}
	if err != nil {
		return HubApplication{}, fmt.Errorf("hub application lookup: %w", err)
	}
	return a, nil
}

func (r *FactionHubRepository) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin hub transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit hub transaction: %w", err)
	}
	return nil
}

// --- reads --------------------------------------------------------------------------------

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// Directory lists the installation's factions (with member counts in the same query),
// newest first. more reports whether another page exists.
func (r *FactionHubRepository) Directory(ctx context.Context, organizationID, installationID int64, q HubDirectoryQuery) (items []HubFaction, more bool, err error) {
	if _, _, err = hubInstallation(ctx, r.pool, organizationID, installationID); err != nil {
		return nil, false, err
	}
	pattern := ""
	if q.Search != "" {
		pattern = "%" + escapeLike(q.Search) + "%"
	}
	limit := q.Limit
	rows, err := r.pool.Query(ctx, `SELECT `+hubFactionCols+`
FROM `+hubFactionFrom+`
WHERE f.organization_id=$1 AND f.installation_id=$2
  AND ($3::boolean = FALSE OR f.recruitment_status = 'OPEN')
  AND ($4::text = '' OR f.name ILIKE $4 ESCAPE '\' OR f.tag ILIKE $4 ESCAPE '\')
  AND ($5::bigint = 0 OR f.id < $5)
ORDER BY f.id DESC
LIMIT $6`, organizationID, installationID, q.RecruitingOnly, pattern, q.AfterID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("hub directory: %w", err)
	}
	defer rows.Close()
	items = []HubFaction{}
	for rows.Next() {
		f, err := scanHubFaction(rows)
		if err != nil {
			return nil, false, fmt.Errorf("hub directory scan: %w", err)
		}
		items = append(items, f)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(items) > limit {
		items, more = items[:limit], true
	}
	return items, more, nil
}

// Get returns one faction's profile with its requirements.
func (r *FactionHubRepository) Get(ctx context.Context, organizationID, installationID, factionID int64) (*HubFaction, error) {
	f, err := hubFactionFull(ctx, r.pool, organizationID, installationID, factionID)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// Members lists a faction's members, most senior first, at most limit; total is the real count.
func (r *FactionHubRepository) Members(ctx context.Context, organizationID, installationID, factionID int64, limit int) (members []HubMember, total int, err error) {
	f, err := hubFactionScoped(ctx, r.pool, organizationID, installationID, factionID, "")
	if err != nil {
		return nil, 0, err
	}
	rows, err := r.pool.Query(ctx, `SELECT `+hubMemberCols+` `+hubMemberFrom+`
WHERE m.faction_id=$1
ORDER BY CASE m.role_key WHEN 'LEADER' THEN 0 WHEN 'OFFICER' THEN 1 ELSE 2 END, m.joined_at, m.id
LIMIT $2`, factionID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("hub members: %w", err)
	}
	defer rows.Close()
	members = []HubMember{}
	for rows.Next() {
		m, err := scanHubMember(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("hub members scan: %w", err)
		}
		members = append(members, m)
	}
	return members, f.MemberCount, rows.Err()
}

// MemberRole returns the user's role in the faction ("" when not a member); the faction
// must exist within the organization and installation.
func (r *FactionHubRepository) MemberRole(ctx context.Context, organizationID, installationID, factionID, userID int64) (string, error) {
	if _, err := hubFactionScoped(ctx, r.pool, organizationID, installationID, factionID, ""); err != nil {
		return "", err
	}
	return hubRole(ctx, r.pool, factionID, userID)
}

// MyFaction returns the user's membership (if any) and pending applications on the installation.
func (r *FactionHubRepository) MyFaction(ctx context.Context, organizationID, installationID, userID int64) (*HubMyFaction, error) {
	if _, _, err := hubInstallation(ctx, r.pool, organizationID, installationID); err != nil {
		return nil, err
	}
	out := &HubMyFaction{Pending: []HubApplication{}}
	var factionID, memberID int64
	err := r.pool.QueryRow(ctx, `SELECT m.faction_id, m.id FROM hub_faction_members m JOIN hub_factions f ON f.id=m.faction_id WHERE m.installation_id=$1 AND m.user_id=$2 AND f.organization_id=$3`, installationID, userID, organizationID).Scan(&factionID, &memberID)
	switch {
	case err == nil:
		f, err := hubFactionFull(ctx, r.pool, organizationID, installationID, factionID)
		if err != nil {
			return nil, err
		}
		m, err := hubMemberByID(ctx, r.pool, factionID, memberID, "")
		if err != nil {
			return nil, err
		}
		out.Faction, out.Member = &f, &m
	case !errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("hub my faction: %w", err)
	}
	rows, err := r.pool.Query(ctx, `SELECT `+hubApplicationCols+` `+hubApplicationFrom+`
WHERE a.installation_id=$1 AND a.user_id=$2 AND a.status='PENDING' AND f.organization_id=$3
ORDER BY a.id DESC`, installationID, userID, organizationID)
	if err != nil {
		return nil, fmt.Errorf("hub my applications: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		a, err := scanHubApplication(rows)
		if err != nil {
			return nil, fmt.Errorf("hub my applications scan: %w", err)
		}
		out.Pending = append(out.Pending, a)
	}
	return out, rows.Err()
}

// ListApplications lists a faction's applications, newest first (optionally one status).
// Only the faction's LEADER/OFFICER - or an organization OWNER/ADMIN viewing for
// moderation - may read them.
func (r *FactionHubRepository) ListApplications(ctx context.Context, organizationID, installationID, factionID int64, access HubAccess, status string, limit int, beforeID int64) (items []HubApplication, more bool, err error) {
	if _, err = hubFactionScoped(ctx, r.pool, organizationID, installationID, factionID, ""); err != nil {
		return nil, false, err
	}
	role, err := hubRole(ctx, r.pool, factionID, access.UserID)
	if err != nil {
		return nil, false, err
	}
	if !factionhub.CanManageApplications(role) && !access.OrgViewer {
		return nil, false, factionhub.ErrForbidden
	}
	rows, err := r.pool.Query(ctx, `SELECT `+hubApplicationCols+` `+hubApplicationFrom+`
WHERE a.faction_id=$1 AND ($2::text = '' OR a.status = $2) AND ($3::bigint = 0 OR a.id < $3)
ORDER BY a.id DESC
LIMIT $4`, factionID, status, beforeID, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("hub applications: %w", err)
	}
	defer rows.Close()
	items = []HubApplication{}
	for rows.Next() {
		a, err := scanHubApplication(rows)
		if err != nil {
			return nil, false, fmt.Errorf("hub applications scan: %w", err)
		}
		items = append(items, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(items) > limit {
		items, more = items[:limit], true
	}
	return items, more, nil
}

// --- faction writes -----------------------------------------------------------------------

// CreateFaction creates a faction and its LEADER membership for userID in one
// transaction. The user must not already be in a faction on the installation; their
// pending applications there are cancelled (they now lead a faction).
func (r *FactionHubRepository) CreateFaction(ctx context.Context, organizationID, installationID, userID int64, in HubFactionInput) (*HubFaction, error) {
	var out HubFaction
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		gameServerID, err := hubWritableInstallation(ctx, tx, organizationID, installationID)
		if err != nil {
			return err
		}
		if err := lockHubUser(ctx, tx, installationID, userID); err != nil {
			return err
		}
		if already, err := hubInstallationMembership(ctx, tx, installationID, userID); err != nil {
			return err
		} else if already {
			return factionhub.ErrAlreadyInFaction
		}
		base := factionhub.Slugify(in.Name)
		var factionID int64
		for n := 1; ; n++ {
			if n > 50 {
				return fmt.Errorf("hub faction slug: no free slug for %q", base)
			}
			err = tx.QueryRow(ctx, `
INSERT INTO hub_factions(organization_id, installation_id, game_server_id, name, tag, slug, description, recruitment_status, created_by_user_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
ON CONFLICT (installation_id, slug) DO NOTHING
RETURNING id`, organizationID, installationID, gameServerID, in.Name, in.Tag, factionhub.SlugCandidate(base, n), in.Description, in.RecruitmentStatus, userID).Scan(&factionID)
			if errors.Is(err, pgx.ErrNoRows) {
				continue // slug taken: try the next suffix
			}
			if err != nil {
				return mapHubUnique(fmt.Errorf("hub create faction: %w", err))
			}
			break
		}
		if _, err := tx.Exec(ctx, `INSERT INTO hub_faction_settings(faction_id) VALUES($1)`, factionID); err != nil {
			return fmt.Errorf("hub create settings: %w", err)
		}
		if _, err := hubAddMember(ctx, tx, factionID, installationID, userID, factionhub.RoleLeader); err != nil {
			return err
		}
		if err := hubCancelPending(ctx, tx, installationID, userID, 0); err != nil {
			return err
		}
		out, err = hubFactionFull(ctx, tx, organizationID, installationID, factionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateFaction replaces the profile fields (LEADER only). The organization,
// installation, server, slug and logo are never changed here (the logo has its own methods); the slug stays stable
// across renames so links keep working.
func (r *FactionHubRepository) UpdateFaction(ctx context.Context, organizationID, installationID, factionID, actorUserID int64, in HubFactionUpdate) (*HubFaction, error) {
	var out HubFaction
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f"); err != nil {
			return err
		}
		role, err := hubRole(ctx, tx, factionID, actorUserID)
		if err != nil {
			return err
		}
		if !factionhub.CanEditFaction(role) {
			return factionhub.ErrForbidden
		}
		primary, secondary := "", ""
		flag, armband := "", ""
		setFlag, setArmband := in.FlagKey != nil, in.ArmbandKey != nil
		if setFlag {
			flag = *in.FlagKey
		}
		if setArmband {
			armband = *in.ArmbandKey
		}
		setPrimary, setSecondary := in.PrimaryColor != nil, in.SecondaryColor != nil
		if setPrimary {
			primary = *in.PrimaryColor
		}
		if setSecondary {
			secondary = *in.SecondaryColor
		}
		if _, err := tx.Exec(ctx, `
UPDATE hub_factions SET name=COALESCE($1,name), tag=COALESCE($2,tag), description=COALESCE($3,description), recruitment_status=COALESCE($4,recruitment_status),
  primary_color   = CASE WHEN $5::boolean THEN NULLIF($6,'') ELSE primary_color END,
  secondary_color = CASE WHEN $7::boolean THEN NULLIF($8,'') ELSE secondary_color END,
  flag_key        = CASE WHEN $9::boolean THEN NULLIF($10,'') ELSE flag_key END,
  armband_key     = CASE WHEN $11::boolean THEN NULLIF($12,'') ELSE armband_key END,
  updated_at=NOW()
WHERE id=$13 AND organization_id=$14 AND installation_id=$15`,
			in.Name, in.Tag, in.Description, in.RecruitmentStatus, setPrimary, primary, setSecondary, secondary, setFlag, flag, setArmband, armband, factionID, organizationID, installationID); err != nil {
			return mapHubUnique(fmt.Errorf("hub update faction: %w", err))
		}
		if s := in.Settings; s != nil {
			if _, err := tx.Exec(ctx, `
INSERT INTO hub_faction_settings(faction_id, minimum_hours, minimum_age, pvp_required, builder_needed, mic_required, custom_requirements, updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,NOW())
ON CONFLICT (faction_id) DO UPDATE SET minimum_hours=EXCLUDED.minimum_hours, minimum_age=EXCLUDED.minimum_age, pvp_required=EXCLUDED.pvp_required,
  builder_needed=EXCLUDED.builder_needed, mic_required=EXCLUDED.mic_required, custom_requirements=EXCLUDED.custom_requirements, updated_at=NOW()`,
				factionID, s.MinimumHours, s.MinimumAge, s.PvPRequired, s.BuilderNeeded, s.MicRequired, s.CustomRequirements); err != nil {
				return fmt.Errorf("hub update settings: %w", err)
			}
		}
		out, err = hubFactionFull(ctx, tx, organizationID, installationID, factionID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// --- applications -------------------------------------------------------------------------

// Apply submits userID's application. The faction must be OPEN, the user in no faction on the
// installation and without a pending application to this faction.
func (r *FactionHubRepository) Apply(ctx context.Context, organizationID, installationID, factionID, userID int64, message string) (*HubApplication, error) {
	var out HubApplication
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubWritableInstallation(ctx, tx, organizationID, installationID); err != nil {
			return err
		}
		if err := lockHubUser(ctx, tx, installationID, userID); err != nil {
			return err
		}
		f, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR SHARE OF f")
		if err != nil {
			return err
		}
		if f.RecruitmentStatus != factionhub.RecruitmentOpen {
			return factionhub.ErrRecruitmentClosed
		}
		if already, err := hubInstallationMembership(ctx, tx, installationID, userID); err != nil {
			return err
		} else if already {
			return factionhub.ErrAlreadyInFaction
		}
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO hub_faction_applications(faction_id, installation_id, user_id, message) VALUES($1,$2,$3,$4) RETURNING id`, factionID, installationID, userID, message).Scan(&id); err != nil {
			return mapHubUnique(fmt.Errorf("hub apply: %w", err))
		}
		out, err = hubApplicationByID(ctx, tx, factionID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// requireManager checks, inside tx, that the actor is a LEADER/OFFICER of the faction.
func requireManager(ctx context.Context, tx pgx.Tx, factionID, actorUserID int64) error {
	var role string
	err := tx.QueryRow(ctx, `SELECT role_key FROM hub_faction_members WHERE faction_id=$1 AND user_id=$2 FOR SHARE`, factionID, actorUserID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return factionhub.ErrForbidden
	}
	if err != nil {
		return fmt.Errorf("hub actor role: %w", err)
	}
	if !factionhub.CanManageApplications(role) {
		return factionhub.ErrForbidden
	}
	return nil
}

// AcceptApplication accepts a pending application in ONE transaction: the actor is a
// LEADER/OFFICER, the application is still PENDING, the applicant is still eligible
// (in no faction on the installation), the membership is created, the application is
// marked ACCEPTED and the applicant's other pending applications on the installation are
// cancelled. Any failure leaves nothing behind.
func (r *FactionHubRepository) AcceptApplication(ctx context.Context, organizationID, installationID, factionID, applicationID, actorUserID int64) (*HubApplication, *HubMember, error) {
	var app HubApplication
	var member HubMember
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, ""); err != nil {
			return err
		}
		if err := requireManager(ctx, tx, factionID, actorUserID); err != nil {
			return err
		}
		peek, err := hubApplicationByID(ctx, tx, factionID, applicationID)
		if err != nil {
			return err
		}
		// The applicant lock comes before the application row lock (see lockHubUser).
		if err := lockHubUser(ctx, tx, installationID, peek.User.ID); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM hub_faction_applications WHERE id=$1 AND faction_id=$2 FOR UPDATE`, applicationID, factionID).Scan(&status); err != nil {
			return fmt.Errorf("hub lock application: %w", err)
		}
		if status != factionhub.ApplicationPending {
			return factionhub.ErrNotPending
		}
		if already, err := hubInstallationMembership(ctx, tx, installationID, peek.User.ID); err != nil {
			return err
		} else if already {
			return factionhub.ErrAlreadyInFaction
		}
		memberID, err := hubAddMember(ctx, tx, factionID, installationID, peek.User.ID, factionhub.RoleMember)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_applications SET status='ACCEPTED', reviewed_by_user_id=$1, reviewed_at=NOW(), updated_at=NOW() WHERE id=$2`, actorUserID, applicationID); err != nil {
			return fmt.Errorf("hub accept application: %w", err)
		}
		if err := hubCancelPending(ctx, tx, installationID, peek.User.ID, applicationID); err != nil {
			return err
		}
		if app, err = hubApplicationByID(ctx, tx, factionID, applicationID); err != nil {
			return err
		}
		member, err = hubMemberByID(ctx, tx, factionID, memberID, "")
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return &app, &member, nil
}

// DenyApplication denies a pending application (LEADER/OFFICER), recording the reviewer.
func (r *FactionHubRepository) DenyApplication(ctx context.Context, organizationID, installationID, factionID, applicationID, actorUserID int64) (*HubApplication, error) {
	var app HubApplication
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, ""); err != nil {
			return err
		}
		if err := requireManager(ctx, tx, factionID, actorUserID); err != nil {
			return err
		}
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM hub_faction_applications WHERE id=$1 AND faction_id=$2 FOR UPDATE`, applicationID, factionID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return factionhub.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("hub lock application: %w", err)
		}
		if status != factionhub.ApplicationPending {
			return factionhub.ErrNotPending
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_applications SET status='DENIED', reviewed_by_user_id=$1, reviewed_at=NOW(), updated_at=NOW() WHERE id=$2`, actorUserID, applicationID); err != nil {
			return fmt.Errorf("hub deny application: %w", err)
		}
		app, err = hubApplicationByID(ctx, tx, factionID, applicationID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// WithdrawApplication lets an applicant withdraw their OWN pending application. Someone
// else's application is reported as not found.
func (r *FactionHubRepository) WithdrawApplication(ctx context.Context, organizationID, installationID, factionID, applicationID, userID int64) (*HubApplication, error) {
	var app HubApplication
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, ""); err != nil {
			return err
		}
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM hub_faction_applications WHERE id=$1 AND faction_id=$2 AND user_id=$3 FOR UPDATE`, applicationID, factionID, userID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return factionhub.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("hub lock application: %w", err)
		}
		if status != factionhub.ApplicationPending {
			return factionhub.ErrNotPending
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_applications SET status='WITHDRAWN', updated_at=NOW() WHERE id=$1`, applicationID); err != nil {
			return fmt.Errorf("hub withdraw application: %w", err)
		}
		app, err = hubApplicationByID(ctx, tx, factionID, applicationID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &app, nil
}

// --- member management --------------------------------------------------------------------

// memberChange runs one member-management action under a lock on the faction row, so two
// management actions on one faction (a demotion racing a removal) are serialized.
func (r *FactionHubRepository) memberChange(ctx context.Context, organizationID, installationID, factionID, memberID, actorUserID int64, fn func(tx pgx.Tx, actorRole string, target HubMember) (*HubMember, error)) (*HubMember, error) {
	var out *HubMember
	err := r.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := hubFactionScoped(ctx, tx, organizationID, installationID, factionID, "FOR UPDATE OF f"); err != nil {
			return err
		}
		actorRole, err := hubRole(ctx, tx, factionID, actorUserID)
		if err != nil {
			return err
		}
		if !factionhub.CanManageApplications(actorRole) { // LEADER or OFFICER: the only roles with any member powers
			return factionhub.ErrForbidden
		}
		target, err := hubMemberByID(ctx, tx, factionID, memberID, "FOR UPDATE OF m")
		if err != nil {
			return err
		}
		out, err = fn(tx, actorRole, target)
		return err
	})
	return out, err
}

func (r *FactionHubRepository) setRole(ctx context.Context, organizationID, installationID, factionID, memberID, actorUserID int64, next func(role string) (string, bool)) (*HubMember, error) {
	return r.memberChange(ctx, organizationID, installationID, factionID, memberID, actorUserID, func(tx pgx.Tx, actorRole string, target HubMember) (*HubMember, error) {
		if !factionhub.CanChangeRoles(actorRole) {
			return nil, factionhub.ErrForbidden
		}
		role, ok := next(target.RoleKey)
		if !ok {
			if target.RoleKey == factionhub.RoleLeader {
				return nil, factionhub.ErrLeaderProtected
			}
			return nil, factionhub.ErrInvalidTransition
		}
		if _, err := tx.Exec(ctx, `UPDATE hub_faction_members SET role_key=$1 WHERE id=$2`, role, memberID); err != nil {
			return nil, fmt.Errorf("hub set role: %w", err)
		}
		m, err := hubMemberByID(ctx, tx, factionID, memberID, "")
		return &m, err
	})
}

// PromoteMember promotes a MEMBER to OFFICER (LEADER only).
func (r *FactionHubRepository) PromoteMember(ctx context.Context, organizationID, installationID, factionID, memberID, actorUserID int64) (*HubMember, error) {
	return r.setRole(ctx, organizationID, installationID, factionID, memberID, actorUserID, factionhub.PromotedRole)
}

// DemoteMember demotes an OFFICER to MEMBER (LEADER only).
func (r *FactionHubRepository) DemoteMember(ctx context.Context, organizationID, installationID, factionID, memberID, actorUserID int64) (*HubMember, error) {
	return r.setRole(ctx, organizationID, installationID, factionID, memberID, actorUserID, factionhub.DemotedRole)
}

// RemoveMember removes a member: the LEADER may remove MEMBER and OFFICER, an OFFICER
// may remove MEMBER only, and the LEADER can never be removed. It returns the removed
// member.
func (r *FactionHubRepository) RemoveMember(ctx context.Context, organizationID, installationID, factionID, memberID, actorUserID int64) (*HubMember, error) {
	return r.memberChange(ctx, organizationID, installationID, factionID, memberID, actorUserID, func(tx pgx.Tx, actorRole string, target HubMember) (*HubMember, error) {
		if target.RoleKey == factionhub.RoleLeader {
			return nil, factionhub.ErrLeaderProtected
		}
		if !factionhub.CanRemove(actorRole, target.RoleKey) {
			return nil, factionhub.ErrForbidden
		}
		if _, err := tx.Exec(ctx, `DELETE FROM hub_faction_members WHERE id=$1`, memberID); err != nil {
			return nil, fmt.Errorf("hub remove member: %w", err)
		}
		return &target, nil
	})
}
