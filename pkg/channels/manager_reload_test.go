package channels

import (
	"context"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

// A changed channel is both removed and added under one name; the reload must
// leave the new instance registered with a live worker.
func TestReloadChangedChannelKeepsWorker(t *testing.T) {
	factoriesMu.Lock()
	prev, hadPrev := factories[config.ChannelMaixCam]
	factoriesMu.Unlock()
	t.Cleanup(func() {
		factoriesMu.Lock()
		defer factoriesMu.Unlock()
		if hadPrev {
			factories[config.ChannelMaixCam] = prev
		} else {
			delete(factories, config.ChannelMaixCam)
		}
	})
	var created []*mockChannel
	RegisterFactory(config.ChannelMaixCam, func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
		ch := &mockChannel{}
		created = append(created, ch)
		return ch, nil
	})

	cfgWith := func(host string) *config.Config {
		cfg := config.DefaultConfig()
		cfg.Channels["cam"] = &config.Channel{
			Enabled:  true,
			Type:     config.ChannelMaixCam,
			Settings: config.RawNode(`{"enabled":true,"host":"` + host + `"}`),
		}
		return cfg
	}

	m := newTestManager()
	ctx := context.Background()
	if err := m.Reload(ctx, cfgWith("a")); err != nil {
		t.Fatalf("first reload: %v", err)
	}
	if err := m.Reload(ctx, cfgWith("b")); err != nil {
		t.Fatalf("second reload: %v", err)
	}

	// Let the deferred registration goroutine finish.
	deadline := time.Now().Add(2 * time.Second)
	for {
		m.mu.RLock()
		ch, w := m.channels["cam"], m.workers["cam"]
		m.mu.RUnlock()
		if len(created) == 2 && ch == Channel(created[1]) && w != nil {
			select {
			case <-w.done:
				t.Fatal("new worker was closed by the reload")
			case <-time.After(100 * time.Millisecond):
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after reload: channel=%v (created %d), worker=%v", ch, len(created), w)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
