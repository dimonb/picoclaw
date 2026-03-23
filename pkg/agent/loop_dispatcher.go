package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
)

var sessionWorkerIdleTimeout = 30 * time.Second

type workerTask struct {
	ctx context.Context
	msg bus.InboundMessage
}

type sessionWorker struct {
	ch      chan workerTask
	mu      sync.Mutex
	closing bool
}

// sessionDispatcher fans out inbound messages to per-session goroutines,
// ensuring messages within a session are processed sequentially while
// different sessions run concurrently.
type sessionDispatcher struct {
	mu        sync.Mutex
	workers   map[string]*sessionWorker
	wg        sync.WaitGroup
	al        *AgentLoop
	semaphore chan struct{} // nil means unlimited
}

func newSessionDispatcher(al *AgentLoop, maxConcurrent int) *sessionDispatcher {
	d := &sessionDispatcher{
		workers: make(map[string]*sessionWorker),
		al:      al,
	}
	if maxConcurrent > 0 {
		d.semaphore = make(chan struct{}, maxConcurrent)
	}
	return d
}

// Dispatch routes msg to the appropriate session worker, creating one if needed.
// Returns immediately; the message is processed asynchronously.
func (d *sessionDispatcher) Dispatch(ctx context.Context, msg bus.InboundMessage) {
	key := d.al.resolveDispatchSessionKey(msg)

	for {
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
		d.mu.Unlock()

		w.mu.Lock()
		if w.closing {
			w.mu.Unlock()
			d.mu.Lock()
			if current, ok := d.workers[key]; ok && current == w {
				delete(d.workers, key)
			}
			d.mu.Unlock()
			continue
		}

		select {
		case w.ch <- workerTask{ctx: ctx, msg: msg}:
			w.mu.Unlock()
			return
		case <-ctx.Done():
			w.mu.Unlock()
			logger.WarnCF("agent", "Dispatcher: context done before enqueue",
				map[string]any{"session_key": key})
			return
		}
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

	for {
		select {
		case task, ok := <-w.ch:
			if !ok {
				// Channel closed by Wait() — drain and exit.
				return
			}
			stopAndDrainTimer(idleTimer)
			d.process(task)
			idleTimer.Reset(sessionWorkerIdleTimeout)

		case <-idleTimer.C:
			d.mu.Lock()
			w.mu.Lock()
			if len(w.ch) > 0 {
				w.mu.Unlock()
				d.mu.Unlock()
				idleTimer.Reset(sessionWorkerIdleTimeout)
				continue
			}
			w.closing = true
			if current, ok := d.workers[key]; ok && current == w {
				delete(d.workers, key)
			}
			w.mu.Unlock()
			d.mu.Unlock()
			return
		}
	}
}

func stopAndDrainTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (d *sessionDispatcher) process(task workerTask) {
	if d.semaphore != nil {
		select {
		case d.semaphore <- struct{}{}:
			defer func() { <-d.semaphore }()
		case <-task.ctx.Done():
			return
		}
	}
	defer func() {
		if d.al.channelManager != nil {
			d.al.channelManager.InvokeTypingStop(task.msg.Channel, task.msg.ChatID)
		}
	}()

	drainCancel := func() {}
	if activeScope, activeAgentID, ok := d.al.resolveSteeringTarget(task.msg); ok {
		drainCtx, cancel := context.WithCancel(task.ctx)
		drainCancel = cancel
		go d.al.drainBusToSteering(drainCtx, activeScope, activeAgentID)
	}
	drainCanceled := false
	cancelDrain := func() {
		if drainCanceled {
			return
		}
		drainCancel()
		drainCanceled = true
	}
	defer cancelDrain()

	response, err := d.al.processMessage(task.ctx, task.msg)
	if err != nil {
		response = agentResponse{Content: fmt.Sprintf("Error processing message: %v", err)}
	}

	finalResponse := response

	target, targetErr := d.al.buildContinuationTarget(task.msg)
	if targetErr != nil {
		logger.WarnCF("agent", "Failed to build steering continuation target", map[string]any{
			"channel": task.msg.Channel,
			"error":   targetErr.Error(),
		})
		d.al.deliverAgentResponse(task.ctx, task.msg.Channel, task.msg.ChatID, finalResponse)
		return
	}
	if target == nil {
		cancelDrain()
		d.al.deliverAgentResponse(task.ctx, task.msg.Channel, task.msg.ChatID, finalResponse)
		return
	}

	for d.al.pendingSteeringCountForScope(target.SessionKey) > 0 {
		logger.InfoCF("agent", "Continuing queued steering after turn end", map[string]any{
			"channel":     target.Channel,
			"chat_id":     target.ChatID,
			"session_key": target.SessionKey,
			"queue_depth": d.al.pendingSteeringCountForScope(target.SessionKey),
		})

		continued, continueErr := d.al.Continue(task.ctx, target.SessionKey, target.Channel, target.ChatID)
		if continueErr != nil {
			logger.WarnCF("agent", "Failed to continue queued steering", map[string]any{
				"channel": target.Channel,
				"chat_id": target.ChatID,
				"error":   continueErr.Error(),
			})
			return
		}
		if continued == "" {
			break
		}

		finalResponse = agentResponse{Content: continued}
	}

	cancelDrain()

	for d.al.pendingSteeringCountForScope(target.SessionKey) > 0 {
		logger.InfoCF("agent", "Draining steering queued during turn shutdown", map[string]any{
			"channel":     target.Channel,
			"chat_id":     target.ChatID,
			"session_key": target.SessionKey,
			"queue_depth": d.al.pendingSteeringCountForScope(target.SessionKey),
		})

		continued, continueErr := d.al.Continue(task.ctx, target.SessionKey, target.Channel, target.ChatID)
		if continueErr != nil {
			logger.WarnCF("agent", "Failed to continue queued steering after shutdown drain", map[string]any{
				"channel": target.Channel,
				"chat_id": target.ChatID,
				"error":   continueErr.Error(),
			})
			return
		}
		if continued == "" {
			break
		}

		finalResponse = agentResponse{Content: continued}
	}

	d.al.deliverAgentResponse(task.ctx, target.Channel, target.ChatID, finalResponse)
}
