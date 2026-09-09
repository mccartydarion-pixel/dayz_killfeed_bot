package panels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Editor interface {
	Send(channelID, content string) (messageID string, err error)
	Edit(channelID, messageID, content string) error
}
type Snapshot struct {
	EventLines  []string
	BountyLines []string
	PointLines  []string
	GeneratedAt time.Time
}
type Loader interface {
	Load(context.Context) (Snapshot, error)
}
type Service struct {
	editor                         Editor
	channelID, messageID, lastHash string
	dirty                          bool
	blocked                        bool
	debounce                       time.Duration
	dirtyAt                        time.Time
	mu                             sync.Mutex
	onMessageID                    func(string)
}

func (s *Service) SetMessageIDHook(fn func(string)) { s.mu.Lock(); s.onMessageID = fn; s.mu.Unlock() }

func NewService(editor Editor, channelID, messageID string) *Service {
	return &Service{editor: editor, channelID: channelID, messageID: messageID, debounce: 15 * time.Second}
}
func (s *Service) MarkDirty()             { s.mu.Lock(); s.dirty = true; s.dirtyAt = time.Now(); s.mu.Unlock() }
func (s *Service) SetMessageID(id string) { s.mu.Lock(); s.messageID = id; s.mu.Unlock() }
func (s *Service) MessageID() string      { s.mu.Lock(); defer s.mu.Unlock(); return s.messageID }
func Render(snapshot Snapshot) string {
	var b strings.Builder
	b.WriteString("🏆 CHAMPION LIVE PANELS\n")
	if len(snapshot.EventLines) > 0 {
		b.WriteString("\n🔥 LIVE EVENT\n")
		for _, line := range snapshot.EventLines {
			fmt.Fprintf(&b, "%s\n", line)
		}
	}
	if len(snapshot.BountyLines) > 0 {
		b.WriteString("\n🎯 MOST WANTED\n")
		for _, line := range snapshot.BountyLines {
			fmt.Fprintf(&b, "%s\n", line)
		}
	}
	if len(snapshot.PointLines) > 0 {
		b.WriteString("\n🏆 CHAMPION POINTS\n")
		for _, line := range snapshot.PointLines {
			fmt.Fprintf(&b, "%s\n", line)
		}
	}
	return b.String()
}
func (s *Service) Flush(snapshot Snapshot, force bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.editor == nil || s.channelID == "" || s.blocked {
		return false, nil
	}
	if !force && (!s.dirty || time.Since(s.dirtyAt) < s.debounce) {
		return false, nil
	}
	content := Render(snapshot)
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	if hash == s.lastHash && s.messageID != "" {
		s.dirty = false
		return false, nil
	}
	var err error
	if s.messageID == "" {
		s.messageID, err = s.editor.Send(s.channelID, content)
		if err == nil && s.onMessageID != nil {
			s.onMessageID(s.messageID)
		}
	} else {
		err = s.editor.Edit(s.channelID, s.messageID, content)
	}
	if err != nil {
		return false, err
	}
	s.lastHash = hash
	s.dirty = false
	return true, nil
}
func (s *Service) Unblock() { s.mu.Lock(); s.blocked = false; s.mu.Unlock() }
func (s *Service) Block()   { s.mu.Lock(); s.blocked = true; s.mu.Unlock() }

type RefreshService struct {
	panel    *Service
	loader   Loader
	interval time.Duration
}

func NewRefreshService(panel *Service, loader Loader) *RefreshService {
	return &RefreshService{panel: panel, loader: loader, interval: 15 * time.Second}
}
func (r *RefreshService) Run(ctx context.Context) {
	if r == nil || r.panel == nil || r.loader == nil {
		return
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			snapshot, err := r.loader.Load(ctx)
			if err == nil {
				_, _ = r.panel.Flush(snapshot, false)
			}
		case <-ctx.Done():
			return
		}
	}
}
