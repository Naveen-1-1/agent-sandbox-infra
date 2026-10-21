package gateway

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	gatewayv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/gateway/v1"
	signalsv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/signals/v1"
)

// Signals is this replica's view of each workspace's traffic: last real
// activity, turns started and ended, open streams, and held requests. The
// controller collects it from every replica for the sleep policy.
type Signals struct {
	gatewayv1.UnimplementedGatewaySignalsServiceServer
	Holder *Holder
	Now    func() time.Time

	mu      sync.Mutex
	started time.Time
	ws      map[string]*wsSignals
}

type wsSignals struct {
	activity, turnStarted, turnEnded time.Time
	streams                          map[signalsv1.StreamKind]int
}

// NewSignals starts an empty view.
func NewSignals(h *Holder) *Signals {
	return &Signals{Holder: h, Now: time.Now, started: time.Now(), ws: map[string]*wsSignals{}}
}

func (s *Signals) get(key string) *wsSignals {
	w := s.ws[key]
	if w == nil {
		w = &wsSignals{streams: map[signalsv1.StreamKind]int{}}
		s.ws[key] = w
	}
	return w
}

// Touch records real activity (an API call made on someone's behalf).
func (s *Signals) Touch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(key).activity = s.Now()
}

// TurnStarted records a user message (wake-hint).
func (s *Signals) TurnStarted(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(key).turnStarted = s.Now()
}

// TurnEnded records that the agent finished its turn.
func (s *Signals) TurnEnded(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(key).turnEnded = s.Now()
}

// OpenStream counts a long-lived connection; call the result when it ends.
func (s *Signals) OpenStream(key string, kind signalsv1.StreamKind) func() {
	s.mu.Lock()
	s.get(key).streams[kind]++
	s.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			w := s.get(key)
			if w.streams[kind]--; w.streams[kind] <= 0 {
				delete(w.streams, kind)
			}
		})
	}
}

// Forget drops a deleted workspace.
func (s *Signals) Forget(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ws, key)
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// GetWorkspaceSignals implements GatewaySignalsService.
func (s *Signals) GetWorkspaceSignals(_ context.Context, r *gatewayv1.GetWorkspaceSignalsRequest) (*gatewayv1.GetWorkspaceSignalsResponse, error) {
	held := map[string]int{}
	if s.Holder != nil {
		held = s.Holder.Held()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := r.GetWorkspaces()
	if len(keys) == 0 {
		for k := range s.ws {
			keys = append(keys, k)
		}
		for k := range held {
			if _, ok := s.ws[k]; !ok {
				keys = append(keys, k)
			}
		}
	}
	out := &gatewayv1.GetWorkspaceSignalsResponse{
		Signals: map[string]*signalsv1.Signals{}, CollectedAt: timestamppb.New(s.Now()), StartedAt: timestamppb.New(s.started),
	}
	for _, k := range keys {
		sig := &signalsv1.Signals{HeldRequests: int32(held[k])} //nolint:gosec // bounded by the hold limits
		if w, ok := s.ws[k]; ok {
			sig.LastActivity, sig.LastTurnStarted, sig.LastTurnEnded = ts(w.activity), ts(w.turnStarted), ts(w.turnEnded)
			for kind, n := range w.streams {
				sig.OpenStreams = append(sig.OpenStreams, &signalsv1.Stream{Kind: kind, Count: int32(n)}) //nolint:gosec // small
			}
		}
		out.Signals[k] = sig
	}
	return out, nil
}
