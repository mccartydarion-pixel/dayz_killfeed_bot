package panels

import (
	"errors"
	"testing"
	"time"
)

type fakeEditor struct {
	sent, edited int
	id           string
	err          error
	editErr      error
	sendErr      error
}

func (f *fakeEditor) Send(string, string) (string, error) {
	f.sent++
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "new", nil
}
func (f *fakeEditor) Edit(string, string, string) error {
	f.edited++
	if f.editErr != nil {
		return f.editErr
	}
	return f.err
}
func TestServiceDebounceAndHash(t *testing.T) {
	f := &fakeEditor{}
	s := NewService(f, "channel", "")
	s.MarkDirty()
	if changed, _ := s.Flush(Snapshot{EventLines: []string{"A"}}, false); changed {
		t.Fatal("must debounce")
	}
	s.mu.Lock()
	s.dirtyAt = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	changed, err := s.Flush(Snapshot{EventLines: []string{"A"}}, false)
	if err != nil || !changed || f.sent != 1 {
		t.Fatalf("first flush: %v %v", changed, err)
	}
	s.MarkDirty()
	s.mu.Lock()
	s.dirtyAt = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	changed, err = s.Flush(Snapshot{EventLines: []string{"A"}}, false)
	if err != nil || changed || f.sent != 1 {
		t.Fatal("unchanged snapshot should skip")
	}
}
func TestServiceBlockedAndRetryable(t *testing.T) {
	f := &fakeEditor{err: errors.New("403")}
	s := NewService(f, "channel", "")
	s.Block()
	s.MarkDirty()
	changed, _ := s.Flush(Snapshot{BountyLines: []string{"A"}}, true)
	if changed || f.sent != 0 {
		t.Fatal("blocked panel must not send")
	}
	s.Unblock()
	f.err = nil
	changed, err := s.Flush(Snapshot{BountyLines: []string{"A"}}, true)
	if err != nil || !changed {
		t.Fatal("unblocked panel should send")
	}
}

func TestServiceRecreatesMissingMessage(t *testing.T) {
	f := &fakeEditor{id: "123"}
	s := NewService(f, "channel", "123")
	s.MarkDirty()
	s.mu.Lock()
	s.dirtyAt = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	f.editErr = &ClassifiedError{Kind: ErrorNotFoundKind, Err: ErrNotFound}
	changed, err := s.Flush(Snapshot{EventLines: []string{"new"}}, false)
	if err != nil || !changed || f.sent != 1 || s.MessageID() != "new" {
		t.Fatalf("recreate failed changed=%v err=%v sent=%d id=%s", changed, err, f.sent, s.MessageID())
	}
}

func TestServiceBlocksPermissionErrors(t *testing.T) {
	f := &fakeEditor{err: &ClassifiedError{Kind: ErrorPermissionKind, Err: ErrPermission}}
	s := NewService(f, "channel", "123")
	s.MarkDirty()
	s.mu.Lock()
	s.dirtyAt = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if _, err := s.Flush(Snapshot{BountyLines: []string{"x"}}, false); err == nil {
		t.Fatal("permission error should be returned")
	}
	s.mu.Lock()
	blocked := s.blocked
	s.mu.Unlock()
	if !blocked {
		t.Fatal("panel should be blocked")
	}
}
