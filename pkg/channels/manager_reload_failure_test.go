package channels

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
)

type reloadTestChannel struct {
	mockChannel
	starts int
	stops  int
}

func (c *reloadTestChannel) Start(context.Context) error {
	c.starts++
	return nil
}

func (c *reloadTestChannel) Stop(context.Context) error {
	c.stops++
	return nil
}

func (c *reloadTestChannel) WebhookPath() string { return "/reload/webhook" }
func (c *reloadTestChannel) HealthPath() string  { return "/reload/health" }

func (c *reloadTestChannel) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusAccepted)
}

func (c *reloadTestChannel) HealthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func setReloadFactory(t *testing.T, factory ChannelFactory) {
	t.Helper()
	factoriesMu.Lock()
	previous, existed := factories[config.ChannelIRC]
	if factory == nil {
		delete(factories, config.ChannelIRC)
	} else {
		factories[config.ChannelIRC] = factory
	}
	factoriesMu.Unlock()
	t.Cleanup(func() {
		factoriesMu.Lock()
		defer factoriesMu.Unlock()
		if existed {
			factories[config.ChannelIRC] = previous
		} else {
			delete(factories, config.ChannelIRC)
		}
	})
}

func reloadConfig(settings string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.Channels = config.ChannelsConfig{
		"chat": &config.Channel{
			Enabled:  true,
			Type:     config.ChannelIRC,
			Settings: config.RawNode(settings),
		},
	}
	return cfg
}

func TestReloadInitializationFailurePreservesActiveChannel(t *testing.T) {
	factoryErr := errors.New("replacement unavailable")
	for _, tc := range []struct {
		name     string
		settings string
		factory  ChannelFactory
		wantErr  string
	}{
		{
			name:     "not ready",
			settings: `{"server":""}`,
			factory: func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
				t.Fatal("factory must not be called for unready settings")
				return nil, nil
			},
			wantErr: "not ready",
		},
		{
			name:     "invalid settings",
			settings: `{"server":42}`,
			wantErr:  "not ready",
		},
		{
			name:     "factory error",
			settings: `{"server":"new.example.invalid"}`,
			factory: func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
				return nil, factoryErr
			},
			wantErr: factoryErr.Error(),
		},
		{
			name:     "missing factory",
			settings: `{"server":"new.example.invalid"}`,
			wantErr:  "not registered",
		},
		{
			name:     "nil factory result",
			settings: `{"server":"new.example.invalid"}`,
			factory: func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
				return nil, nil
			},
			wantErr: "returned nil",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldChannel := &reloadTestChannel{}
			setReloadFactory(t, func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
				return oldChannel, nil
			})
			oldConfig := reloadConfig(`{"server":"old.example.invalid"}`)
			m, err := NewManager(oldConfig, bus.NewMessageBus(), nil)
			if err != nil {
				t.Fatal(err)
			}
			m.mux = newDynamicServeMux()
			m.registerHTTPHandlersLocked()
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if startErr := m.StartAll(ctx); startErr != nil {
				t.Fatal(startErr)
			}
			t.Cleanup(func() { _ = m.StopAll(context.Background()) })
			oldWorker := m.workers["chat"]
			oldDispatchTask := m.dispatchTask
			oldHashes := toChannelHashes(oldConfig)
			setReloadFactory(t, tc.factory)
			newConfig := reloadConfig(tc.settings)

			err = m.Reload(ctx, newConfig)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("reload error = %v, want %q", err, tc.wantErr)
			}
			if tc.name == "factory error" && !errors.Is(err, factoryErr) {
				t.Fatalf("reload error does not wrap factory error: %v", err)
			}
			if m.channels["chat"] != oldChannel || m.workers["chat"] != oldWorker {
				t.Fatal("failed reload replaced the active channel or worker")
			}
			if oldChannel.starts != 1 || oldChannel.stops != 0 {
				t.Fatalf("old channel lifecycle: starts=%d stops=%d", oldChannel.starts, oldChannel.stops)
			}
			if m.config != oldConfig || !reflect.DeepEqual(m.channelHashes, oldHashes) {
				t.Fatal("failed reload committed config or hashes")
			}
			if m.dispatchTask != oldDispatchTask {
				t.Fatal("failed reload replaced the dispatch task")
			}
			for path, status := range map[string]int{
				oldChannel.WebhookPath(): http.StatusAccepted,
				oldChannel.HealthPath():  http.StatusNoContent,
			} {
				recorder := httptest.NewRecorder()
				m.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
				if recorder.Code != status {
					t.Fatalf("handler %s returned %d, want %d", path, recorder.Code, status)
				}
			}
			select {
			case <-oldWorker.done:
				t.Fatal("old worker stopped after failed reload")
			case <-oldWorker.mediaDone:
				t.Fatal("old media worker stopped after failed reload")
			default:
			}
			feedback := make(chan bus.DeliveryResult, 1)
			oldWorker.queue <- bus.OutboundMessage{ChatID: "room", Content: "still active", Feedback: feedback}
			select {
			case result := <-feedback:
				if result.Err != nil {
					t.Fatalf("old worker delivery failed: %v", result.Err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("old worker did not deliver after failed reload")
			}
		})
	}
}

func TestReloadRejectsUnreadyNewChannel(t *testing.T) {
	m := newTestManager()
	oldConfig := config.DefaultConfig()
	m.config = oldConfig
	if err := m.Reload(context.Background(), reloadConfig(`{"server":""}`)); err == nil {
		t.Fatal("expected an error for an unready new channel")
	}
	if len(m.channels) != 0 || len(m.workers) != 0 || len(m.channelHashes) != 0 || m.config != oldConfig {
		t.Fatal("failed reload modified the empty manager")
	}
}

func TestReloadRemovesChannelSkippedAtStartup(t *testing.T) {
	cfg := reloadConfig(`{"server":""}`)
	m, err := NewManager(cfg, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.channels) != 0 || len(m.channelHashes) != 1 {
		t.Fatal("expected a hash without an initialized channel")
	}
	if err := m.Reload(context.Background(), config.DefaultConfig()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll(context.Background()) })
	if len(m.channels) != 0 || len(m.channelHashes) != 0 {
		t.Fatal("removed channel remains configured")
	}
}

func TestReloadRetriesFailedReplacement(t *testing.T) {
	oldChannel := &reloadTestChannel{}
	setReloadFactory(t, func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
		return oldChannel, nil
	})
	oldConfig := reloadConfig(`{"server":"old.example.invalid"}`)
	m, err := NewManager(oldConfig, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll(context.Background()) })
	oldWorker := m.workers["chat"]
	newConfig := reloadConfig(`{"server":"new.example.invalid"}`)
	setReloadFactory(t, func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
		return nil, errors.New("temporarily unavailable")
	})
	if err := m.Reload(ctx, newConfig); err == nil {
		t.Fatal("expected the first reload to fail")
	}

	replacement := &reloadTestChannel{}
	setReloadFactory(t, func(_ string, _ string, cfg *config.Config, _ *bus.MessageBus) (Channel, error) {
		if cfg != newConfig {
			t.Fatal("factory did not receive the requested config")
		}
		if oldChannel.stops != 0 {
			t.Fatal("old channel stopped before its replacement was constructed")
		}
		return replacement, nil
	})
	if err := m.Reload(ctx, newConfig); err != nil {
		t.Fatalf("retry with the same config: %v", err)
	}
	if m.channels["chat"] != replacement || m.workers["chat"].ch != replacement {
		t.Fatal("retry did not install the replacement channel and worker")
	}
	if m.config != newConfig || !reflect.DeepEqual(m.channelHashes, toChannelHashes(newConfig)) {
		t.Fatal("successful retry did not commit config and hashes")
	}
	if oldChannel.stops != 1 || replacement.starts != 1 {
		t.Fatalf("retry lifecycle: old stops=%d new starts=%d", oldChannel.stops, replacement.starts)
	}
	for _, done := range []<-chan struct{}{oldWorker.done, oldWorker.mediaDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("old worker was not drained after successful replacement")
		}
	}
	newWorker := m.workers["chat"]
	select {
	case <-newWorker.done:
		t.Fatal("replacement worker stopped")
	case <-newWorker.mediaDone:
		t.Fatal("replacement media worker stopped")
	default:
	}
}

func TestReloadPreparationFailurePreventsOtherRemovals(t *testing.T) {
	oldChannel := &reloadTestChannel{}
	setReloadFactory(t, func(string, string, *config.Config, *bus.MessageBus) (Channel, error) {
		return oldChannel, nil
	})
	oldConfig := reloadConfig(`{"server":"old.example.invalid"}`)
	m, err := NewManager(oldConfig, bus.NewMessageBus(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.StartAll(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopAll(context.Background()) })
	oldWorker := m.workers["chat"]
	newConfig := reloadConfig(`{"server":""}`)
	newConfig.Channels["new"] = newConfig.Channels["chat"]
	delete(newConfig.Channels, "chat")

	if err := m.Reload(ctx, newConfig); err == nil {
		t.Fatal("expected new channel initialization to fail")
	}
	if m.channels["chat"] != oldChannel || m.workers["chat"] != oldWorker || oldChannel.stops != 0 {
		t.Fatal("failed preparation removed an unrelated active channel")
	}
	if m.config != oldConfig || !reflect.DeepEqual(m.channelHashes, toChannelHashes(oldConfig)) {
		t.Fatal("failed preparation committed config or hashes")
	}
}
