package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const sessionWorkerIdleTimeout = 30 * time.Second

type workerTask struct {
	ctx context.Context
	msg bus.InboundMessage
}

type sessionWorker struct {
	ch    chan workerTask
	timer *time.Timer
}

// sessionDispatcher fans out inbound messages to per-session goroutines,
// ensuring messages within a session are processed sequentially while
// different sessions run concurrently.
type sessionDispatcher struct {
	mu      sync.Mutex
	workers map[string]*sessionWorker
	wg      sync.WaitGroup
	al      *AgentLoop
}

func newSessionDispatcher(al *AgentLoop) *sessionDispatcher {
	return &sessionDispatcher{
		workers: make(map[string]*sessionWorker),
		al:      al,
	}
}

// Dispatch routes msg to the appropriate session worker, creating one if needed.
// Returns immediately; the message is processed asynchronously.
func (d *sessionDispatcher) Dispatch(ctx context.Context, msg bus.InboundMessage) {
	key := msg.SessionKey

	d.mu.Lock()
	w, ok := d.workers[key]
	if !ok {
		w = &sessionWorker{
			ch: make(chan workerTask, 32),
		}
		d.workers[key] = w
		d.wg.Add(1)
		go d.runWorker(key, w)
	}
	// Reset idle timer so the worker doesn't exit while we have work.
	if w.timer != nil {
		w.timer.Reset(sessionWorkerIdleTimeout)
	}
	d.mu.Unlock()

	select {
	case w.ch <- workerTask{ctx: ctx, msg: msg}:
	case <-ctx.Done():
		logger.WarnCF("agent", "Dispatcher: context done before enqueue",
			map[string]any{"session_key": key})
	}
}

// Wait blocks until all session workers have finished.
func (d *sessionDispatcher) Wait() {
	d.mu.Lock()
	for _, w := range d.workers {
		close(w.ch)
	}
	d.mu.Unlock()
	d.wg.Wait()
}

func (d *sessionDispatcher) runWorker(key string, w *sessionWorker) {
	defer d.wg.Done()

	idleTimer := time.NewTimer(sessionWorkerIdleTimeout)
	defer idleTimer.Stop()
	w.timer = idleTimer

	for {
		select {
		case task, ok := <-w.ch:
			if !ok {
				// Channel closed by Wait() — drain and exit.
				return
			}
			idleTimer.Reset(sessionWorkerIdleTimeout)
			d.process(task)

		case <-idleTimer.C:
			// No messages for a while — clean up and exit.
			d.mu.Lock()
			delete(d.workers, key)
			d.mu.Unlock()
			return
		}
	}
}

func (d *sessionDispatcher) process(task workerTask) {
	response, err := d.al.processMessage(task.ctx, task.msg)
	if err != nil {
		response = agentResponse{Content: fmt.Sprintf("Error processing message: %v", err)}
	}
	d.al.deliverAgentResponse(task.ctx, task.msg.Channel, task.msg.ChatID, response)
}
