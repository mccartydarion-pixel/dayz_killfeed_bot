package repository

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FeatureSettingsRepository stores each installation's opt-in feature settings
// (installation_feature_settings): hot zones, public fight replay, the cross-server network listing
// and the feed identity. Every feature here is off until the installation turns it on; an
// installation with no row reads as DefaultFeatureSettings.
type FeatureSettingsRepository struct{ pool *pgxpool.Pool }

func NewFeatureSettingsRepository(pool *pgxpool.Pool) *FeatureSettingsRepository {
	return &FeatureSettingsRepository{pool: pool}
}

// HotZoneSettings configures heatmap-driven hot-zone events (docs/HOT_ZONES.md).
type HotZoneSettings struct {
	Enabled         bool
	WindowMinutes   int // how far back kills are counted
	MinKills        int // kills in one grid cell that open a hot zone
	RadiusM         int // scoring radius around the cell centre
	DurationMinutes int
	CooldownMinutes int // quiet time after a hot zone ends before the next may open
	FirstPoints     int
	SecondPoints    int
	ThirdPoints     int
}

// FightReplaySettings controls who may watch fight replays (docs/FIGHT_REPLAY.md). Staff with the
// location capability always can; Public opens them to the server's verified players, but only
// for fights at least DelayMinutes old.
type FightReplaySettings struct {
	Public       bool
	DelayMinutes int
}

// NetworkSettings controls the installation's listing in the cross-server network
// (docs/NETWORK.md). Nothing about an unlisted installation is ever returned by a network route.
type NetworkSettings struct {
	Listed      bool
	Description string
	// DiscordInviteURL is the owner's own invite to the server's Discord, shown on the public
	// listing. Empty, or https://discord.gg/<code>.
	DiscordInviteURL string
}

// FeedIdentitySettings is the name and avatar feed messages are posted under (docs/FEED_IDENTITY.md).
type FeedIdentitySettings struct {
	Enabled   bool
	Name      string
	AvatarURL string
}

// FeatureSettings is one installation's full set.
type FeatureSettings struct {
	InstallationID int64
	HotZones       HotZoneSettings
	FightReplay    FightReplaySettings
	Network        NetworkSettings
	FeedIdentity   FeedIdentitySettings
	UpdatedAt      *time.Time
}

// DefaultFeatureSettings is what an installation that never saved anything has: everything off,
// with the tuning values the features use once enabled.
func DefaultFeatureSettings(installationID int64) FeatureSettings {
	return FeatureSettings{
		InstallationID: installationID,
		HotZones: HotZoneSettings{WindowMinutes: 60, MinKills: 6, RadiusM: 500, DurationMinutes: 30, CooldownMinutes: 120,
			FirstPoints: 500, SecondPoints: 250, ThirdPoints: 100},
		FightReplay: FightReplaySettings{DelayMinutes: 60},
	}
}

// ErrInvalidFeatureSettings wraps a caller-fixable validation message.
var ErrInvalidFeatureSettings = errors.New("invalid feature settings")

func invalidSetting(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidFeatureSettings, fmt.Sprintf(format, args...))
}

func between(name string, v, min, max int) error {
	if v < min || v > max {
		return invalidSetting("%s must be between %d and %d", name, min, max)
	}
	return nil
}

// Validate checks the hot-zone tuning values. The same bounds are CHECK constraints on the table.
func (s HotZoneSettings) Validate() error {
	for _, c := range []struct {
		name        string
		v, min, max int
	}{
		{"windowMinutes", s.WindowMinutes, 15, 360}, {"minKills", s.MinKills, 2, 500}, {"radiusMeters", s.RadiusM, 100, 2000},
		{"durationMinutes", s.DurationMinutes, 5, 240}, {"cooldownMinutes", s.CooldownMinutes, 0, 1440},
		{"firstPoints", s.FirstPoints, 0, 1000000}, {"secondPoints", s.SecondPoints, 0, 1000000}, {"thirdPoints", s.ThirdPoints, 0, 1000000},
	} {
		if err := between(c.name, c.v, c.min, c.max); err != nil {
			return err
		}
	}
	return nil
}

func (s FightReplaySettings) Validate() error {
	return between("delayMinutes", s.DelayMinutes, 0, 10080)
}

// discordInvite matches the forms Discord hands out for an invite: discord.gg/<code>,
// discord.com/invite/<code> and the older discordapp.com/invite/<code>, with or without a scheme
// or "www.". Anything after the code (?event=..., a trailing slash) is dropped.
var discordInvite = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?(?:discord\.gg(?:/invite)?|discord(?:app)?\.com/invite)/([A-Za-z0-9-]{2,64})/?(?:[?#].*)?$`)

// NormalizeDiscordInvite rewrites any accepted invite form to https://discord.gg/<code>. ok is
// false when raw is not a Discord invite; an empty raw is valid and stays empty.
func NormalizeDiscordInvite(raw string) (normalized string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	m := discordInvite.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	return "https://discord.gg/" + m[1], true
}

// Normalize trims the description and rewrites the invite to its canonical form; an invite that
// is not one is left as typed for Validate to reject.
func (s NetworkSettings) Normalize() NetworkSettings {
	s.Description = strings.TrimSpace(s.Description)
	s.DiscordInviteURL = strings.TrimSpace(s.DiscordInviteURL)
	if n, ok := NormalizeDiscordInvite(s.DiscordInviteURL); ok {
		s.DiscordInviteURL = n
	}
	return s
}

// Validate bounds the description and accepts only a Discord invite as the link: the listing is
// public, so it must never carry an arbitrary URL.
func (s NetworkSettings) Validate() error {
	if utf8.RuneCountInString(s.Description) > 280 {
		return invalidSetting("description must be 280 characters or fewer")
	}
	if n, ok := NormalizeDiscordInvite(s.DiscordInviteURL); !ok || n != s.DiscordInviteURL {
		return invalidSetting("discordInviteUrl must be a Discord invite link, like https://discord.gg/yourcode")
	}
	return nil
}

func (s FeedIdentitySettings) Normalize() FeedIdentitySettings {
	s.Name, s.AvatarURL = strings.TrimSpace(s.Name), strings.TrimSpace(s.AvatarURL)
	return s
}

// Validate applies Discord's webhook username rules (1-80 characters, never containing "discord"
// or "clyde", no @ # : or backticks) and requires an https avatar URL when one is given.
func (s FeedIdentitySettings) Validate() error {
	if !s.Enabled && s.Name == "" && s.AvatarURL == "" {
		return nil
	}
	n := utf8.RuneCountInString(s.Name)
	if s.Enabled && n == 0 {
		return invalidSetting("name is required when the feed identity is enabled")
	}
	if n > 80 {
		return invalidSetting("name must be 80 characters or fewer")
	}
	lower := strings.ToLower(s.Name)
	if strings.Contains(lower, "discord") || strings.Contains(lower, "clyde") {
		return invalidSetting(`name may not contain "discord" or "clyde"`)
	}
	if strings.ContainsAny(s.Name, "@#:`") || strings.EqualFold(s.Name, "everyone") || strings.EqualFold(s.Name, "here") {
		return invalidSetting("name may not contain @, #, : or backticks")
	}
	if s.AvatarURL != "" {
		u, err := url.Parse(s.AvatarURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || len(s.AvatarURL) > 512 {
			return invalidSetting("avatarUrl must be an https URL of 512 characters or fewer")
		}
	}
	return nil
}

const featureSettingsColumns = `installation_id, hot_zones_enabled, hot_zone_window_minutes, hot_zone_min_kills, hot_zone_radius_m,
    hot_zone_duration_minutes, hot_zone_cooldown_minutes, hot_zone_first_points, hot_zone_second_points, hot_zone_third_points,
    fight_replay_public, fight_replay_delay_minutes, network_listed, network_description, network_discord_invite_url,
    feed_identity_enabled, feed_identity_name, feed_identity_avatar_url, updated_at`

func scanFeatureSettings(row pgx.Row) (FeatureSettings, error) {
	var s FeatureSettings
	var updated time.Time
	err := row.Scan(&s.InstallationID, &s.HotZones.Enabled, &s.HotZones.WindowMinutes, &s.HotZones.MinKills, &s.HotZones.RadiusM,
		&s.HotZones.DurationMinutes, &s.HotZones.CooldownMinutes, &s.HotZones.FirstPoints, &s.HotZones.SecondPoints, &s.HotZones.ThirdPoints,
		&s.FightReplay.Public, &s.FightReplay.DelayMinutes, &s.Network.Listed, &s.Network.Description, &s.Network.DiscordInviteURL,
		&s.FeedIdentity.Enabled, &s.FeedIdentity.Name, &s.FeedIdentity.AvatarURL, &updated)
	s.UpdatedAt = &updated
	return s, err
}

// Get returns the installation's settings, or the defaults when it never saved any.
func (r *FeatureSettingsRepository) Get(ctx context.Context, installationID int64) (FeatureSettings, error) {
	s, err := scanFeatureSettings(r.pool.QueryRow(ctx, `SELECT `+featureSettingsColumns+` FROM installation_feature_settings WHERE installation_id=$1`, installationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return DefaultFeatureSettings(installationID), nil
	}
	return s, err
}

// save upserts one section's columns, leaving the others at their stored (or default) values.
func (r *FeatureSettingsRepository) save(ctx context.Context, installationID, userID int64, columns []string, values ...any) (FeatureSettings, error) {
	placeholders := make([]string, len(columns))
	sets := make([]string, len(columns))
	for i, c := range columns {
		placeholders[i] = fmt.Sprintf("$%d", i+3)
		sets[i] = c + "=EXCLUDED." + c
	}
	q := `INSERT INTO installation_feature_settings(installation_id, updated_by_user_id, ` + strings.Join(columns, ", ") + `)
VALUES($1, NULLIF($2, 0), ` + strings.Join(placeholders, ", ") + `)
ON CONFLICT (installation_id) DO UPDATE SET ` + strings.Join(sets, ", ") + `, updated_by_user_id=EXCLUDED.updated_by_user_id, updated_at=NOW()
RETURNING ` + featureSettingsColumns
	return scanFeatureSettings(r.pool.QueryRow(ctx, q, append([]any{installationID, userID}, values...)...))
}

func (r *FeatureSettingsRepository) SaveHotZones(ctx context.Context, installationID, userID int64, s HotZoneSettings) (FeatureSettings, error) {
	if err := s.Validate(); err != nil {
		return FeatureSettings{}, err
	}
	return r.save(ctx, installationID, userID, []string{"hot_zones_enabled", "hot_zone_window_minutes", "hot_zone_min_kills", "hot_zone_radius_m",
		"hot_zone_duration_minutes", "hot_zone_cooldown_minutes", "hot_zone_first_points", "hot_zone_second_points", "hot_zone_third_points"},
		s.Enabled, s.WindowMinutes, s.MinKills, s.RadiusM, s.DurationMinutes, s.CooldownMinutes, s.FirstPoints, s.SecondPoints, s.ThirdPoints)
}

func (r *FeatureSettingsRepository) SaveFightReplay(ctx context.Context, installationID, userID int64, s FightReplaySettings) (FeatureSettings, error) {
	if err := s.Validate(); err != nil {
		return FeatureSettings{}, err
	}
	return r.save(ctx, installationID, userID, []string{"fight_replay_public", "fight_replay_delay_minutes"}, s.Public, s.DelayMinutes)
}

func (r *FeatureSettingsRepository) SaveNetwork(ctx context.Context, installationID, userID int64, s NetworkSettings) (FeatureSettings, error) {
	s = s.Normalize()
	if err := s.Validate(); err != nil {
		return FeatureSettings{}, err
	}
	return r.save(ctx, installationID, userID, []string{"network_listed", "network_description", "network_discord_invite_url"}, s.Listed, s.Description, s.DiscordInviteURL)
}

func (r *FeatureSettingsRepository) SaveFeedIdentity(ctx context.Context, installationID, userID int64, s FeedIdentitySettings) (FeatureSettings, error) {
	s = s.Normalize()
	if err := s.Validate(); err != nil {
		return FeatureSettings{}, err
	}
	return r.save(ctx, installationID, userID, []string{"feed_identity_enabled", "feed_identity_name", "feed_identity_avatar_url"}, s.Enabled, s.Name, s.AvatarURL)
}

// HotZoneInstallation is one installation with hot zones enabled and a server selected.
type HotZoneInstallation struct {
	InstallationID int64
	GuildID        int64
	ServerID       int64
	ServerName     string
	Settings       HotZoneSettings
}

// HotZoneInstallations lists the guild's installations that have hot zones enabled.
func (r *FeatureSettingsRepository) HotZoneInstallations(ctx context.Context, guildID int64) ([]HotZoneInstallation, error) {
	rows, err := r.pool.Query(ctx, `
SELECT i.id, c.guild_id, i.game_server_id, COALESCE(gs.display_name, ''),
       s.hot_zone_window_minutes, s.hot_zone_min_kills, s.hot_zone_radius_m, s.hot_zone_duration_minutes, s.hot_zone_cooldown_minutes,
       s.hot_zone_first_points, s.hot_zone_second_points, s.hot_zone_third_points
FROM installation_feature_settings s
JOIN installations i ON i.id = s.installation_id AND i.game_server_id IS NOT NULL
JOIN discord_guild_connections c ON c.id = i.discord_guild_connection_id
JOIN game_servers gs ON gs.id = i.game_server_id
WHERE c.guild_id = $1 AND s.hot_zones_enabled
ORDER BY i.id`, guildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HotZoneInstallation
	for rows.Next() {
		h := HotZoneInstallation{Settings: HotZoneSettings{Enabled: true}}
		if err := rows.Scan(&h.InstallationID, &h.GuildID, &h.ServerID, &h.ServerName, &h.Settings.WindowMinutes, &h.Settings.MinKills, &h.Settings.RadiusM,
			&h.Settings.DurationMinutes, &h.Settings.CooldownMinutes, &h.Settings.FirstPoints, &h.Settings.SecondPoints, &h.Settings.ThirdPoints); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// FeedIdentityForServer returns the enabled feed identity of the installation backed by serverID.
// ok is false when no installation of that server has one enabled.
func (r *FeatureSettingsRepository) FeedIdentityForServer(ctx context.Context, serverID int64) (FeedIdentitySettings, bool, error) {
	var s FeedIdentitySettings
	err := r.pool.QueryRow(ctx, `
SELECT s.feed_identity_name, s.feed_identity_avatar_url FROM installation_feature_settings s
JOIN installations i ON i.id = s.installation_id
WHERE i.game_server_id = $1 AND s.feed_identity_enabled AND s.feed_identity_name <> ''
ORDER BY i.id LIMIT 1`, serverID).Scan(&s.Name, &s.AvatarURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return FeedIdentitySettings{}, false, nil
	}
	if err != nil {
		return FeedIdentitySettings{}, false, err
	}
	s.Enabled = true
	return s, true, nil
}
