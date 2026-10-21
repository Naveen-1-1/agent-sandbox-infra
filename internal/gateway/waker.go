package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"golang.org/x/sync/singleflight"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sbxv1 "github.com/Naveen-1-1/agent-sandbox-infra/api/v1alpha1"
)

// ErrOff means the workspace's mode is Off, so requests never wake it.
var ErrOff = errors.New("gateway: workspace is off")

// Waker asks the controller for a wake by bumping spec.wake.seq. A bump is
// only made when no wake is pending (spec.wake.seq == status.observedWakeSeq)
// and with a resourceVersion precondition, so concurrent requests, on one
// replica or many, add up to exactly one wake.
type Waker struct {
	Client client.Client // writes
	API    client.Reader // uncached reads
	Now    func() time.Time

	flight singleflight.Group
}

// Wake requests a wake for ws if it needs one. It reports whether this call
// made the request (false: already awake, waking or pending).
func (w *Waker) Wake(ctx context.Context, key client.ObjectKey, trigger sbxv1.WakeTrigger) (bool, error) {
	v, err, _ := w.flight.Do(key.String()+"/"+string(trigger), func() (any, error) {
		for range 5 {
			ws := &sbxv1.Workspace{}
			if err := w.API.Get(ctx, key, ws); err != nil {
				return false, err
			}
			if ws.Spec.Mode == sbxv1.ModeOff {
				return false, ErrOff
			}
			if !NeedsWake(ws) {
				return false, nil
			}
			now := time.Now
			if w.Now != nil {
				now = w.Now
			}
			patch, err := json.Marshal(map[string]any{
				"metadata": map[string]any{"resourceVersion": ws.ResourceVersion},
				"spec": map[string]any{"wake": map[string]any{
					"seq": ws.Spec.Wake.Seq + 1, "trigger": trigger, "at": now().UTC().Format(time.RFC3339Nano),
				}},
			})
			if err != nil {
				return false, err
			}
			err = w.Client.Patch(ctx, ws, client.RawPatch(types.MergePatchType, patch))
			if apierrors.IsConflict(err) {
				continue // someone else changed it; look again
			}
			return err == nil, err
		}
		return false, errors.New("gateway: workspace kept changing while requesting a wake")
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

// NeedsWake reports whether ws is asleep (or falling asleep) with no wake
// already requested.
func NeedsWake(ws *sbxv1.Workspace) bool {
	if ws.Spec.Wake.Seq > ws.Status.ObservedWakeSeq {
		return false // pending
	}
	switch ws.Status.Phase {
	case sbxv1.PhaseSuspending, sbxv1.PhaseTearingDown, sbxv1.PhaseSuspended:
		return true
	}
	return false
}

// Ready reports whether ws can serve requests now.
func Ready(ws *sbxv1.Workspace) bool {
	return ws.Status.Phase == sbxv1.PhaseRunning && ws.Status.Pod != nil && ws.Status.Pod.IP != "" &&
		ws.Status.Suspend == nil
}
