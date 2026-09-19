package assetstore

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidKey(t *testing.T) {
	good := []string{"factions/1/2/3/0a1b2c3d-0000-4000-8000-000000000000.png", "a", "logos/x_y-z.webp", "a/b/c"}
	for _, k := range good {
		if !ValidKey(k) {
			t.Errorf("%q must be valid", k)
		}
	}
	bad := []string{"", "/etc/passwd", "../secret", "a/../b", "a//b", "a/b/", "a\\b", "a b", "a?b", "a%2e%2e/b", "a\x00b", "C:\\x", ".hidden", "-x", "a/./b/..", strings.Repeat("a", MaxKeyLen+1), "caf\u00e9/x"}
	for _, k := range bad {
		if ValidKey(k) {
			t.Errorf("%q must be rejected", k)
		}
	}
}

func TestMemoryStoreContract(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if err := s.Put(ctx, "../evil", "image/png", []byte("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("traversal must be refused: %v", err)
	}
	if err := s.Put(ctx, "factions/1/1/1/a.png", "image/png", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	data, ct, err := s.Get(ctx, "factions/1/1/1/a.png")
	if err != nil || string(data) != "abc" || ct != "image/png" {
		t.Fatalf("get: %q %q %v", data, ct, err)
	}
	data[0] = 'X' // callers cannot mutate stored bytes
	if again, _, _ := s.Get(ctx, "factions/1/1/1/a.png"); string(again) != "abc" {
		t.Fatal("Get must return a copy")
	}
	if _, _, err := s.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := s.Delete(ctx, "missing"); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
	if err := s.Delete(ctx, "factions/1/1/1/a.png"); err != nil || s.Has("factions/1/1/1/a.png") {
		t.Fatal("delete")
	}
	if _, ok := s.URL("x"); ok {
		t.Fatal("the memory store has no public URL")
	}
}

func TestMemoryStoreListFiltersByPrefixAndAge(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	now := time.Now()
	s.Now = func() time.Time { return now.Add(-3 * time.Hour) }
	_ = s.Put(ctx, "factions/old.png", "image/png", []byte("1"))
	s.Now = func() time.Time { return now }
	_ = s.Put(ctx, "factions/new.png", "image/png", []byte("1"))
	_ = s.Put(ctx, "other/old.png", "image/png", []byte("1"))
	keys, _ := s.List(ctx, "factions/", now.Add(-time.Hour), 10)
	if len(keys) != 1 || keys[0] != "factions/old.png" {
		t.Fatalf("list: %v", keys)
	}
	s.PutErr = errors.New("boom")
	if err := s.Put(ctx, "factions/x.png", "image/png", []byte("1")); err == nil {
		t.Fatal("injected put failure")
	}
}
