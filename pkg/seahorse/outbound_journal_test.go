package seahorse

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOutboundJournalFailureRestartReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	e, err := NewEngine(Config{DBPath: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := e.store
	// Abort only archive writes, leaving the schema and receipt files usable.
	_, err = s.db.Exec(
		`CREATE TRIGGER fail_receipt BEFORE INSERT ON outbound_deliveries BEGIN SELECT RAISE(FAIL,'forced outage'); END`,
	)
	if err != nil {
		t.Fatal(err)
	}
	d := OutboundDelivery{Channel: "telegram", MessageID: "-100:1:2", Content: "originaldurable"}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.RecordOutboundDelivery(canceled, d); err == nil {
		t.Fatal("expected queued error")
	}
	d.Content = "latestdurable"
	if err = s.RecordOutboundDelivery(context.Background(), d); err == nil {
		t.Fatal("expected queued edit")
	}
	entries, err := os.ReadDir(path + ".outbound-receipts")
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries=%v err=%v", entries, err)
	}
	info, _ := os.Stat(path + ".outbound-receipts")
	if info.Mode().Perm() != 0o700 {
		t.Fatal(info.Mode())
	}
	for _, entry := range entries {
		info, _ := os.Stat(filepath.Join(path+".outbound-receipts", entry.Name()))
		if info.Mode().Perm() != 0o600 {
			t.Fatal(info.Mode())
		}
	}
	// A fully written but unpublished temp receipt must not be replayed.
	if err = os.WriteFile(
		filepath.Join(path+".outbound-receipts", ".receipt-incomplete"),
		[]byte("{"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`DROP TRIGGER fail_receipt`); err != nil {
		t.Fatal(err)
	}
	e.Close()
	e, err = NewEngine(Config{DBPath: path}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s = e.store
	got, err := s.GetMessageByChannelMessageID(context.Background(), d.MessageID)
	if err != nil || got == nil || got.Content != d.Content {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	created := got.CreatedAt
	r := &RetrievalEngine{store: s}
	found, err := r.Grep(
		context.Background(),
		GrepInput{Pattern: "latestdurable", Scope: "message", AllConversations: true},
	)
	if err != nil || len(found.Messages) != 1 {
		t.Fatalf("search=%+v err=%v", found, err)
	}
	d.Content = "lastedit"
	if err = s.RecordOutboundDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetMessageByChannelMessageID(context.Background(), d.MessageID)
	if err != nil || !got.CreatedAt.Equal(created) {
		t.Fatalf("timestamp changed: %+v %v", got, err)
	}
	var n int
	s.db.QueryRow("SELECT count(*) FROM outbound_deliveries").Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
}

func TestOutboundJournalSubsequentRecordAndConcurrentEdits(t *testing.T) {
	e, err := NewEngine(Config{DBPath: filepath.Join(t.TempDir(), "a.db")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := e.store
	s.db.Exec(`CREATE TRIGGER fail_receipt BEFORE INSERT ON outbound_deliveries BEGIN SELECT RAISE(FAIL,'forced'); END`)
	d := OutboundDelivery{Channel: "telegram", MessageID: "ref", Content: "old"}
	if err = s.RecordOutboundDelivery(context.Background(), d); err == nil {
		t.Fatal("expected failure")
	}
	s.db.Exec(`DROP TRIGGER fail_receipt`)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := d
			v.Content = "concurrent"
			if recordErr := s.RecordOutboundDelivery(context.Background(), v); recordErr != nil {
				t.Error(recordErr)
			}
		}()
	}
	wg.Wait()
	d.Content = "newest"
	if err = s.RecordOutboundDelivery(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessageByChannelMessageID(context.Background(), "ref")
	if err != nil || got == nil || got.Content != "newest" {
		t.Fatalf("%+v %v", got, err)
	}
	entries, _ := os.ReadDir(s.outboundJournal.dir)
	if len(entries) != 0 {
		t.Fatal(entries)
	}
}

func TestOutboundJournalDiskAndDatabaseFailure(t *testing.T) {
	e, err := NewEngine(Config{DBPath: filepath.Join(t.TempDir(), "a.db")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := e.store
	e.Close()
	os.Remove(s.outboundJournal.dir)
	if err = os.WriteFile(s.outboundJournal.dir, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordOutboundDelivery(
		context.Background(),
		OutboundDelivery{Channel: "telegram", MessageID: "ref"},
	); err == nil {
		t.Fatal("both storage paths failed silently")
	}
}
