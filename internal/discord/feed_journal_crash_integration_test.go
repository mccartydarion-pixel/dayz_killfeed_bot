//go:build integration

package discord

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/yourname/dayz-killfeed/internal/database"
	"github.com/yourname/dayz-killfeed/internal/repository"
)

// Crash-loss regression test with a REAL crash: a child process running an
// immediate-mode feed with the Postgres journal is killed with SIGKILL (no
// shutdown, no final flush) while three cards are queued - the first one
// already created in Discord, its confirmation never received. A new process
// on the same journal must then deliver all three exactly once, in order.
//
// Before the journal (096f90e) the same kill loses the queued cards; see the
// "crash" part of TestImmediateFailureRecovery.

const crashChildEnv = "FEED_CRASH_CHILD"

func crashDB(t *testing.T) *database.DB {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	if os.Getenv("ALLOW_INTEGRATION_DB_TESTS") != "true" {
		t.Fatal("set ALLOW_INTEGRATION_DB_TESTS=true for an explicit non-production integration database")
	}
	db, err := database.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db
}

func openCards(t *testing.T, j *repository.FeedCardRepository, key string) []repository.FeedCard {
	t.Helper()
	cards, err := j.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return cards
}

// TestJournalCrashChildProcess is the process that gets killed. It only runs
// when started by TestJournalSurvivesSIGKILL.
func TestJournalCrashChildProcess(t *testing.T) {
	if os.Getenv(crashChildEnv) != "1" {
		t.Skip("child process of TestJournalSurvivesSIGKILL")
	}
	discordgo.EndpointChannels = os.Getenv("FEED_CRASH_ENDPOINT")
	db := crashDB(t)
	ch := os.Getenv("FEED_CRASH_CHANNEL")
	s := newTestSession(t, time.Minute)
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: ch})
	f := NewRotatingFeed(NewFeedSession(s), store, "g1", func(g *GuildSetup) string { return g.KillfeedChannelID }, time.Hour, 10)
	f.SetMode(FeedModeImmediate)
	f.SetJournal(repository.NewFeedCardRepository(db.Pool), os.Getenv("FEED_CRASH_KEY"))
	go f.Run(context.Background())
	for n := 1; n <= 3; n++ {
		f.EnqueueDetected(card(n), time.Now())
	}
	select {} // killed by the parent
}

func TestJournalSurvivesSIGKILL(t *testing.T) {
	if os.Getenv(crashChildEnv) == "1" {
		t.Skip("parent only")
	}
	db := crashDB(t)
	freshLedger(t)
	j := repository.NewFeedCardRepository(db.Pool)
	key := fmt.Sprintf("KILLFEED:crash-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM discord_feed_cards WHERE feed_key=$1`, key)
	})

	emu := newDiscordEmu()
	emu.hangFor = time.Hour // the first create lands in Discord; its reply never arrives
	ch := testChannel(t, emu, "crash-kf")

	child := exec.Command(os.Args[0], "-test.run=^TestJournalCrashChildProcess$", "-test.count=1")
	child.Env = append(os.Environ(), crashChildEnv+"=1", "FEED_CRASH_ENDPOINT="+discordgo.EndpointChannels,
		"FEED_CRASH_CHANNEL="+ch, "FEED_CRASH_KEY="+key)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for !(len(openCards(t, j, key)) == 3 && emu.count(emu.creates, ch) == 1) {
		if time.Now().After(deadline) {
			t.Fatalf("child never reached the crash point: journal=%d creates=%d", len(openCards(t, j, key)), emu.count(emu.creates, ch))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// SIGKILL: no ctx cancel, no final flush, no deferred code.
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = child.Process.Wait()
	killed = true
	for _, c := range openCards(t, j, key) {
		if c.MessageID != "" {
			t.Fatalf("setup: no card may be confirmed before the crash: %+v", c)
		}
	}
	if got := emu.titles(ch); !reflect.DeepEqual(got, titlesRange(1, 1)) {
		t.Fatalf("setup: expected only kill-01 created before the crash, channel %v", got)
	}

	// New process (here: a new feed in this process) on the same journal.
	emu.mu.Lock()
	emu.hangFor = 0
	emu.mu.Unlock()
	s := newTestSession(t, 5*time.Second)
	store := NewInMemorySetupStore()
	_ = store.Save(GuildSetup{GuildID: "g1", KillfeedChannelID: ch})
	f := NewRotatingFeed(NewFeedSession(s), store, "g1", func(g *GuildSetup) string { return g.KillfeedChannelID }, time.Hour, 10)
	f.SetMode(FeedModeImmediate)
	f.SetJournal(j, key)
	ctx, cancel := context.WithCancel(context.Background())
	go f.Run(ctx)
	t.Cleanup(func() { cancel(); f.WaitDone() })

	eventually(t, "all three queued cards delivered after the crash", func() bool {
		return reflect.DeepEqual(emu.titles(ch), titlesRange(1, 3))
	})
	if c := emu.count(emu.creates, ch); c != 3 {
		t.Fatalf("expected exactly 3 cards created (kill-01 not duplicated), got %d", c)
	}
	eventually(t, "journal confirms all three", func() bool {
		cards := openCards(t, j, key)
		if len(cards) != 3 {
			return false
		}
		for _, c := range cards {
			if c.MessageID == "" {
				return false
			}
		}
		return true
	})
	if r := ledger("KILLFEED"); r.Replayed != 3 {
		t.Fatalf("ledger replayed %d, want 3", r.Replayed)
	}
}
