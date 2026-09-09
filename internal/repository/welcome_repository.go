package repository

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"time"
)

type WelcomeConfig struct {
	GuildID                                                                         int64
	Enabled                                                                         bool
	ChannelID, MessageText, TitleText, FooterText, ImageURL, ThumbnailURL           string
	Color                                                                           *int
	MentionUser, WelcomeBots, ShowMemberCount, ShowServerName, ShowLinkInstructions bool
	LastWelcomeAt                                                                   *time.Time
}
type WelcomeRepository struct{ pool *pgxpool.Pool }

func NewWelcomeRepository(pool *pgxpool.Pool) *WelcomeRepository {
	return &WelcomeRepository{pool: pool}
}
func (r *WelcomeRepository) Get(ctx context.Context, guildID int64) (*WelcomeConfig, error) {
	var c WelcomeConfig
	err := r.pool.QueryRow(ctx, `SELECT guild_id,enabled,COALESCE(channel_id,''),COALESCE(message_text,''),COALESCE(title_text,''),COALESCE(footer_text,''),COALESCE(image_url,''),COALESCE(thumbnail_url,''),color,mention_user,welcome_bots,show_member_count,show_server_name,show_link_instructions,last_welcome_at FROM guild_welcome_configs WHERE guild_id=$1`, guildID).Scan(&c.GuildID, &c.Enabled, &c.ChannelID, &c.MessageText, &c.TitleText, &c.FooterText, &c.ImageURL, &c.ThumbnailURL, &c.Color, &c.MentionUser, &c.WelcomeBots, &c.ShowMemberCount, &c.ShowServerName, &c.ShowLinkInstructions, &c.LastWelcomeAt)
	return &c, err
}
func (r *WelcomeRepository) Upsert(ctx context.Context, c WelcomeConfig) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO guild_welcome_configs(guild_id,enabled,channel_id,message_text,title_text,footer_text,image_url,thumbnail_url,color,mention_user,welcome_bots,show_member_count,show_server_name,show_link_instructions) VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10,$11,$12,$13,$14) ON CONFLICT(guild_id) DO UPDATE SET enabled=EXCLUDED.enabled,channel_id=EXCLUDED.channel_id,message_text=EXCLUDED.message_text,title_text=EXCLUDED.title_text,footer_text=EXCLUDED.footer_text,image_url=EXCLUDED.image_url,thumbnail_url=EXCLUDED.thumbnail_url,color=EXCLUDED.color,mention_user=EXCLUDED.mention_user,welcome_bots=EXCLUDED.welcome_bots,show_member_count=EXCLUDED.show_member_count,show_server_name=EXCLUDED.show_server_name,show_link_instructions=EXCLUDED.show_link_instructions,updated_at=NOW()`, c.GuildID, c.Enabled, c.ChannelID, c.MessageText, c.TitleText, c.FooterText, c.ImageURL, c.ThumbnailURL, c.Color, c.MentionUser, c.WelcomeBots, c.ShowMemberCount, c.ShowServerName, c.ShowLinkInstructions)
	return err
}
func (r *WelcomeRepository) MarkWelcomeSent(ctx context.Context, guildID int64, at time.Time) error {
	_, err := r.pool.Exec(ctx, `UPDATE guild_welcome_configs SET last_welcome_at=$2,updated_at=NOW() WHERE guild_id=$1`, guildID, at)
	return err
}
