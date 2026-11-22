package seahorse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
)

const outboundReceiptTimeout = 10 * time.Second

type outboundReceipt struct {
	Delivery OutboundDelivery `json:"delivery"`
	Created  string           `json:"created_at"`
}

// One writer (the engine) owns this private spool. The mutex orders confirmations
// and edits, including replay; an older queued edit must never replace a newer one.
type outboundJournal struct {
	mu       sync.Mutex
	dir      string
	sequence int64
}

func syncReceiptDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func openOutboundJournal(dir string) (*outboundJournal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("not a private receipt directory: %s", dir)
	}
	if err = os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if err = syncReceiptDir(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	j := &outboundJournal{dir: dir, sequence: time.Now().UnixNano()}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			n, err := strconv.ParseInt(strings.TrimSuffix(e.Name(), ".json"), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid receipt filename: %s", e.Name())
			}
			if n >= j.sequence {
				j.sequence = n + 1
			}
		}
	}
	return j, nil
}

func (j *outboundJournal) enqueue(d OutboundDelivery) error {
	b, err := json.Marshal(outboundReceipt{Delivery: d, Created: time.Now().UTC().Format(sqliteTimeLayout)})
	if err != nil {
		return err
	}
	// An incomplete temp file is never replayed. Rename publishes only a fully
	// synced receipt, then directory sync makes its name durable before SQL.
	f, err := os.CreateTemp(j.dir, ".receipt-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	j.sequence++
	name := filepath.Join(j.dir, fmt.Sprintf("%020d.json", j.sequence))
	if err := os.Rename(tmp, name); err != nil {
		return err
	}
	return syncReceiptDir(j.dir)
}

func (j *outboundJournal) replay(s *Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), outboundReceiptTimeout)
	defer cancel()
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(a, b int) bool { return entries[a].Name() < entries[b].Name() })
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(j.dir, e.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("unsafe receipt file %s", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r outboundReceipt
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("decode receipt %s: %w", path, err)
		}
		if r.Created == "" || r.Delivery.Channel == "" || r.Delivery.MessageID == "" {
			return fmt.Errorf("invalid receipt %s", path)
		}
		if err := s.insertOutboundDelivery(ctx, legacyReceiptOrigin(r.Delivery), r.Created); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		// Stop if cleanup is not durable, so later edits cannot be overtaken by an
		// older file resurrected after a crash.
		if err := syncReceiptDir(j.dir); err != nil {
			return err
		}
	}
	return nil
}

// legacyReceiptOrigin sets the origin of a receipt queued by a version from
// before OutboundDelivery.Origin, by the same rule as the database migration
// (ensureOutboundDeliveryOriginColumn): with a session key it is a send
// (edits never carried one); without, it stays unknown and is not trusted.
func legacyReceiptOrigin(d OutboundDelivery) OutboundDelivery {
	if d.Origin == "" && d.SessionKey != "" {
		d.Origin = bus.OutboundOriginSend
	}
	return d
}

func (j *outboundJournal) record(s *Store, d OutboundDelivery) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.enqueue(d); err != nil {
		// If the spool is unavailable, still try durable SQL, but only after older
		// receipts have been reconciled. Never claim success if both paths fail.
		if replayErr := j.replay(s); replayErr != nil {
			return errors.Join(fmt.Errorf("journal receipt: %w", err), replayErr)
		}
		ctx, cancel := context.WithTimeout(context.Background(), outboundReceiptTimeout)
		defer cancel()
		if dbErr := s.insertOutboundDelivery(ctx, d, time.Now().UTC().Format(sqliteTimeLayout)); dbErr != nil {
			return errors.Join(err, dbErr)
		}
		return nil
	}
	if err := j.replay(s); err != nil {
		return fmt.Errorf("receipt durably queued; archive replay pending: %w", err)
	}
	return nil
}
