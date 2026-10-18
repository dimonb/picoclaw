package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type debtContextManager struct {
	noopContextManager
	history []providers.Message
	calls   chan CompactRequest
	release chan struct{}
	done    chan error
}

func (m *debtContextManager) Assemble(context.Context, *AssembleRequest) (*AssembleResponse, error) {
	return &AssembleResponse{Evicted: true, History: m.history}, nil
}
func (m *debtContextManager) Compact(ctx context.Context, req *CompactRequest) error {
	m.calls <- *req
	if req.Reason == ContextCompressReasonProactive {
		return context.DeadlineExceeded
	}
	<-m.release
	m.done <- ctx.Err()
	return nil
}

func TestPipelineDeferredCompactionDebt(t *testing.T) {
	for _, hard := range []bool{false, true} {
		for _, handled := range []bool{false, true} {
			for _, noHistory := range []bool{false, true} {
				t.Run(fmt.Sprintf("hard=%v/handled=%v/noHistory=%v", hard, handled, noHistory), func(t *testing.T) {
					al, agent, cleanup := newTurnCoordTestLoop(t, &simpleConvProvider{})
					defer cleanup()
					agent.ContextWindow = 160000
					agent.MaxTokens = 4000
					m := &debtContextManager{calls: make(chan CompactRequest, 4), release: make(chan struct{}), done: make(chan error, 1)}
					defer close(m.release)
					if hard {
						m.history = []providers.Message{{Role: "user", Content: strings.Repeat("old conversation ", 40000)}}
					}
					al.contextManager = m
					p := NewPipeline(al)
					opts := makeTestProcessOpts("debt-test")
					opts.NoHistory = noHistory
					opts.EnableSummary = false
					ts := newTurnState(agent, opts, turnEventScope{turnID: "debt", context: newTurnContext(nil, nil, nil)})
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					exec, err := p.SetupTurn(ctx, ts)
					if err != nil {
						t.Fatal(err)
					}
					if noHistory {
						if exec.deferredCompactBudget != 0 {
							t.Fatal("NoHistory accrued debt")
						}
					} else {
						if exec.deferredCompactBudget <= 0 {
							t.Fatal("missing maintenance debt")
						}
						if hard {
							select {
							case req := <-m.calls:
								if req.Reason != ContextCompressReasonProactive {
									t.Fatalf("unexpected %+v", req)
								}
							default:
								t.Fatal("hard overflow did not attempt proactive fallback")
							}
							if estimateContextBudgetStats(agent.ContextWindow, exec.messages, exec.providerToolDefs, agent.MaxTokens).OverBudget {
								t.Fatal("timeout fallback did not fit history")
							}
						}
					}
					select {
					case req := <-m.calls:
						t.Fatalf("fitting request waited for compaction: %+v", req)
					default:
					}
					exec.allResponsesHandled = handled
					if _, err = p.Finalize(ctx, ctx, ts, exec, TurnEndStatusCompleted, "answer"); err != nil {
						t.Fatal(err)
					}
					cancel() // scheduled maintenance must survive cancellation of this turn
					if noHistory {
						select {
						case req := <-m.calls:
							t.Fatalf("NoHistory compacted: %+v", req)
						case <-time.After(30 * time.Millisecond):
						}
						return
					}
					select {
					case req := <-m.calls:
						if req.Reason != ContextCompressReasonSummarize || req.HistoryBudget != exec.deferredCompactBudget {
							t.Fatalf("wrong debt snapshot: %+v", req)
						}
					case <-time.After(time.Second):
						t.Fatal("maintenance was not scheduled with EnableSummary=false")
					}
					m.release <- struct{}{}
					select {
					case err := <-m.done:
						if err != nil {
							t.Fatalf("turn cancellation killed maintenance: %v", err)
						}
					case <-time.After(time.Second):
						t.Fatal("maintenance stuck")
					}
				})
			}
		}
	}
}
