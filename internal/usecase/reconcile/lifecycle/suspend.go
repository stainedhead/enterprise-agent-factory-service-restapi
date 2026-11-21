package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// WorkloadState is what a hook records about one workload before suspend scales it down, so
// resume restores exactly that: an agent paused before the suspend stays paused (RC-US-15.1).
type WorkloadState struct {
	// Kind is "agent" or "pool".
	Kind string `json:"kind"`
	// ID is the workload id.
	ID string `json:"id"`
	// Replicas is the replica count before suspend.
	Replicas int32 `json:"replicas"`
	// Paused is true when the workload was paused by its own desired state before the suspend.
	Paused bool `json:"paused"`
}

// ScaleHook is implemented by the agent (M2) and pool (M5) controllers.
type ScaleHook interface {
	// Name identifies the hook in conditions ("Agents", "Pools").
	Name() string
	// Suspend scales every workload of the team to zero and returns the state it found. It is
	// idempotent: a workload already at zero because of an earlier suspend is returned from the
	// stored prior state by the caller, so Suspend must report only live workloads.
	Suspend(ctx context.Context, teamID string) ([]WorkloadState, error)
	// Resume restores each listed workload to its recorded state.
	Resume(ctx context.Context, teamID string, prior []WorkloadState) error
}

// PriorStateStore keeps the recorded states between the suspend and the resume. The reconciler's
// adapter stores them on the team's status (annotation or a status-side table); the fake is a map.
type PriorStateStore interface {
	Save(ctx context.Context, teamID string, states map[string]json.RawMessage) error
	Load(ctx context.Context, teamID string) (map[string]json.RawMessage, error)
	Clear(ctx context.Context, teamID string) error
}

// SuspendResume runs the hooks for a team.
type SuspendResume struct {
	Hooks []ScaleHook
	Store PriorStateStore
}

// Suspend scales every workload down and records the prior state before returning. A suspended
// team's earlier record is never overwritten by a second suspend pass (the workloads are already
// at zero then): hooks that report nothing keep the stored state.
func (s *SuspendResume) Suspend(ctx context.Context, teamID string) error {
	stored, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return fmt.Errorf("lifecycle: load prior state: %w", err)
	}
	if stored == nil {
		stored = map[string]json.RawMessage{}
	}
	var errs []error
	for _, h := range s.hooks() {
		states, err := h.Suspend(ctx, teamID)
		if err != nil {
			errs = append(errs, fmt.Errorf("suspend %s: %w", h.Name(), err))
			continue
		}
		if len(states) == 0 {
			continue
		}
		raw, err := json.Marshal(merge(stored[h.Name()], states))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		stored[h.Name()] = raw
		// Saved after each hook, so a crash between hooks loses nothing already scaled down.
		if err := s.Store.Save(ctx, teamID, stored); err != nil {
			errs = append(errs, fmt.Errorf("save prior state: %w", err))
		}
	}
	return errors.Join(errs...)
}

// Resume restores every workload to its recorded state and clears the record once all hooks
// succeeded; a failed hook leaves the record so the next pass retries.
func (s *SuspendResume) Resume(ctx context.Context, teamID string) error {
	stored, err := s.Store.Load(ctx, teamID)
	if err != nil {
		return fmt.Errorf("lifecycle: load prior state: %w", err)
	}
	var errs []error
	for _, h := range s.hooks() {
		var prior []WorkloadState
		if raw, ok := stored[h.Name()]; ok {
			if err := json.Unmarshal(raw, &prior); err != nil {
				errs = append(errs, fmt.Errorf("prior state of %s: %w", h.Name(), err))
				continue
			}
		}
		if err := h.Resume(ctx, teamID, prior); err != nil {
			errs = append(errs, fmt.Errorf("resume %s: %w", h.Name(), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return s.Store.Clear(ctx, teamID)
}

func (s *SuspendResume) hooks() []ScaleHook {
	h := append([]ScaleHook(nil), s.Hooks...)
	sort.SliceStable(h, func(i, j int) bool { return h[i].Name() < h[j].Name() })
	return h
}

// merge keeps the states already recorded for a workload and adds new ones.
func merge(old json.RawMessage, fresh []WorkloadState) []WorkloadState {
	var cur []WorkloadState
	if len(old) > 0 {
		_ = json.Unmarshal(old, &cur)
	}
	have := map[string]bool{}
	for _, c := range cur {
		have[c.Kind+"/"+c.ID] = true
	}
	for _, f := range fresh {
		if !have[f.Kind+"/"+f.ID] {
			cur = append(cur, f)
		}
	}
	return cur
}
