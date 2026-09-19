// Package factionassets orchestrates faction logo storage (docs/FACTIONS.md): validate the
// image, write the bytes to the durable assetstore.Store, record the metadata and repoint the
// faction transactionally, and only then remove the previous bytes. It owns the ordering that
// keeps a failed step from losing a logo, and the orphan sweep that keeps a failed step from
// leaking bytes. Authorization lives in the repository (the LEADER check runs inside the same
// transaction that repoints the faction); this package never decides who may do what.
package factionassets

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/yourname/dayz-killfeed/internal/assetstore"
	"github.com/yourname/dayz-killfeed/internal/factionhub"
	"github.com/yourname/dayz-killfeed/internal/logoimage"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// KeyPrefix is the storage-key prefix of every faction asset.
const KeyPrefix = "factions/"

// ErrTypeMismatch: the client declared a Content-Type that does not match the image bytes.
var ErrTypeMismatch = errors.New("the declared content type does not match the image")

// ErrStorage: the byte store failed. The caller answers 500 and nothing was recorded.
var ErrStorage = errors.New("asset storage failed")

// Repo is the metadata persistence the service needs (*repository.FactionHubRepository).
type Repo interface {
	RequireLeader(ctx context.Context, organizationID, installationID, factionID, userID int64) error
	ReplaceLogo(ctx context.Context, organizationID, installationID, factionID, actorUserID int64, in repository.NewLogoAsset) (factionhub.Asset, *factionhub.Asset, error)
	DeleteLogo(ctx context.Context, organizationID, installationID, factionID, actorUserID int64) (*factionhub.Asset, error)
	AssetByPublicID(ctx context.Context, publicID string) (*factionhub.Asset, error)
	ReferencedAssetKeys(ctx context.Context, keys []string) (map[string]bool, error)
}

// Service stores, replaces, deletes and serves faction logos.
type Service struct {
	store assetstore.Store
	repo  Repo
	now   func() time.Time
}

func NewService(store assetstore.Store, repo Repo) *Service {
	return &Service{store: store, repo: repo, now: time.Now}
}

// RequireLeader is the cheap pre-check the upload handler runs before reading the body.
func (s *Service) RequireLeader(ctx context.Context, organizationID, installationID, factionID, userID int64) error {
	return s.repo.RequireLeader(ctx, organizationID, installationID, factionID, userID)
}

// UploadInput is one logo upload. Data is the raw file (already size-limited by the caller).
type UploadInput struct {
	OrganizationID, InstallationID, FactionID, ActorUserID int64
	Data                                                   []byte
	DeclaredType                                           string // the multipart part's Content-Type, "" if absent
	Filename                                               string // as sent; sanitized before it is kept
}

// UploadResult is the stored logo and whether it replaced an earlier one.
type UploadResult struct {
	Asset    factionhub.Asset
	Replaced bool
}

// UploadLogo validates data as a real PNG/JPEG/WebP, stores it, records it and repoints the
// faction. Order matters and is what makes a failure safe:
//  1. validate the bytes (nothing is stored for a bad image);
//  2. write the NEW bytes to the store (the old logo is still in place and referenced);
//  3. one transaction: LEADER check, insert metadata, repoint the faction, drop the old row;
//     if it fails the new bytes are deleted (or, if that fails, left for the orphan sweep);
//  4. only after the commit delete the OLD bytes (failure leaves an orphan for the sweep).
func (s *Service) UploadLogo(ctx context.Context, in UploadInput) (UploadResult, error) {
	info, err := logoimage.Inspect(in.Data)
	if err != nil {
		return UploadResult{}, err
	}
	if d := normalizeType(in.DeclaredType); d != "" && d != info.ContentType {
		return UploadResult{}, ErrTypeMismatch
	}
	publicID, err := newUUID()
	if err != nil {
		return UploadResult{}, fmt.Errorf("generate asset id: %w", err)
	}
	key := fmt.Sprintf("%s%d/%d/%d/%s.%s", KeyPrefix, in.OrganizationID, in.InstallationID, in.FactionID, publicID, info.Ext)
	if !assetstore.ValidKey(key) {
		return UploadResult{}, fmt.Errorf("generated an invalid storage key")
	}
	if err := s.store.Put(ctx, key, info.ContentType, in.Data); err != nil {
		slog.Warn("component=faction_assets", "msg", "store put failed", "err", err.Error())
		return UploadResult{}, ErrStorage
	}
	asset, old, err := s.repo.ReplaceLogo(ctx, in.OrganizationID, in.InstallationID, in.FactionID, in.ActorUserID, repository.NewLogoAsset{
		PublicID: publicID, StorageKey: key, ContentType: info.ContentType, OriginalFilename: SanitizeFilename(in.Filename),
		SizeBytes: len(in.Data), Width: info.Width, Height: info.Height,
	})
	if err != nil {
		s.deleteBestEffort(key, "rolled back upload")
		return UploadResult{}, err
	}
	if old != nil {
		s.deleteBestEffort(old.StorageKey, "replaced logo")
	}
	return UploadResult{Asset: asset, Replaced: old != nil}, nil
}

// DeleteLogo removes the faction's logo. Idempotent: it returns (nil, nil) when there was none.
func (s *Service) DeleteLogo(ctx context.Context, organizationID, installationID, factionID, actorUserID int64) (*factionhub.Asset, error) {
	old, err := s.repo.DeleteLogo(ctx, organizationID, installationID, factionID, actorUserID)
	if err != nil {
		return nil, err
	}
	if old != nil {
		s.deleteBestEffort(old.StorageKey, "deleted logo")
	}
	return old, nil
}

// Served is a logo ready to send to a browser.
type Served struct {
	Asset       factionhub.Asset
	Data        []byte
	ContentType string
}

// Serve resolves a public logo id to its bytes. A replaced or deleted logo has no metadata row,
// so its old URL is factionhub.ErrNotFound.
func (s *Service) Serve(ctx context.Context, publicID string) (*Served, error) {
	asset, err := s.repo.AssetByPublicID(ctx, publicID)
	if err != nil {
		return nil, err
	}
	data, _, err := s.store.Get(ctx, asset.StorageKey)
	if errors.Is(err, assetstore.ErrNotFound) {
		slog.Warn("component=faction_assets", "msg", "asset metadata without bytes", "asset_id", asset.ID)
		return nil, factionhub.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read asset: %w", err)
	}
	// The recorded (validated) type is authoritative, not whatever the store returned.
	return &Served{Asset: *asset, Data: data, ContentType: asset.ContentType}, nil
}

func (s *Service) deleteBestEffort(key, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.Delete(ctx, key); err != nil {
		// Not fatal: the object is unreferenced and the orphan sweep will remove it.
		slog.Warn("component=faction_assets", "msg", "could not delete asset bytes; left for the orphan sweep", "reason", why, "err", err.Error())
	}
}

// SweepOrphans deletes stored objects under KeyPrefix that are older than minAge and not
// referenced by any asset row: bytes left behind by a failed delete, a rolled-back upload whose
// cleanup also failed, or a deleted faction. minAge keeps it away from an upload that has
// written its bytes but not yet committed its metadata. It returns how many it deleted.
func (s *Service) SweepOrphans(ctx context.Context, minAge time.Duration, batch, maxBatches int) (int, error) {
	if batch <= 0 {
		batch = 200
	}
	if maxBatches <= 0 {
		maxBatches = 1
	}
	deleted := 0
	cutoff := s.now().Add(-minAge)
	for i := 0; i < maxBatches; i++ {
		keys, err := s.store.List(ctx, KeyPrefix, cutoff, batch)
		if err != nil {
			return deleted, fmt.Errorf("list assets: %w", err)
		}
		if len(keys) == 0 {
			return deleted, nil
		}
		referenced, err := s.repo.ReferencedAssetKeys(ctx, keys)
		if err != nil {
			return deleted, err
		}
		removedThisBatch := 0
		for _, k := range keys {
			if referenced[k] {
				continue
			}
			if err := s.store.Delete(ctx, k); err != nil {
				slog.Warn("component=faction_assets", "msg", "orphan delete failed", "err", err.Error())
				continue
			}
			removedThisBatch++
		}
		deleted += removedThisBatch
		if removedThisBatch == 0 || len(keys) < batch {
			return deleted, nil // nothing more to make progress on
		}
	}
	return deleted, nil
}

// RunSweeper runs SweepOrphans every interval until ctx ends. Never panics the process.
func (s *Service) RunSweeper(ctx context.Context, interval, minAge time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("component=faction_assets", "msg", "orphan sweep panic recovered", "panic", fmt.Sprint(r))
					}
				}()
				sweepCtx, cancel := context.WithTimeout(ctx, time.Minute)
				defer cancel()
				n, err := s.SweepOrphans(sweepCtx, minAge, 200, 5)
				if err != nil {
					slog.Warn("component=faction_assets", "msg", "orphan sweep failed", "err", err.Error())
				} else if n > 0 {
					slog.Info("component=faction_assets", "event", "orphan_assets_swept", "count", n)
				}
			}()
		}
	}
}

// normalizeType reduces a declared Content-Type to a bare media type; "" and the generic
// application/octet-stream (some clients send it for every file) mean "not declared".
func normalizeType(declared string) string {
	t := strings.ToLower(strings.TrimSpace(declared))
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	if t == "application/octet-stream" || t == "binary/octet-stream" {
		return ""
	}
	if t == "image/jpg" || t == "image/pjpeg" {
		return logoimage.JPEG
	}
	return t
}

// SanitizeFilename reduces a client-supplied file name to a harmless display string: base name
// only (no directories, either separator), [A-Za-z0-9._ -] only, no leading dots, at most 100
// characters. The result is stored for display/debugging and is NEVER used as a path.
func SanitizeFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == ' ':
			b.WriteRune(r)
		}
		if b.Len() >= 100 {
			break
		}
	}
	out := strings.Trim(strings.Join(strings.Fields(b.String()), " "), ". ")
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// newUUID returns a random (version 4) UUID string.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0F | 0x40
	b[8] = b[8]&0x3F | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
