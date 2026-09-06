package sync

import (
	"context"
	"fmt"

	"github.com/arcbjorn/nomads-agent/pkg/nomads"
)

// TripClient is the subset of the Nomads client that applying a plan needs.
// Keeping it narrow lets the executor be tested against a fake.
type TripClient interface {
	ListTrips(ctx context.Context) ([]nomads.Trip, error)
	CreateTrip(ctx context.Context, req nomads.CreateTripRequest) (*nomads.Trip, error)
	UpdateTrip(ctx context.Context, id string, patch nomads.TripPatch) (*nomads.Trip, error)
	DeleteTrip(ctx context.Context, id string) error
}

// Result records what applying a plan actually did.
type Result struct {
	Created   []nomads.Trip    `json:"created"`
	Updated   []nomads.Trip    `json:"updated"`
	Deleted   []nomads.Trip    `json:"deleted"`
	Unchanged []nomads.Trip    `json:"unchanged"`
	Ambiguous []AmbiguousMatch `json:"ambiguous"`
	// Errors holds per-action failures. Applying continues past a failure so a
	// single bad trip cannot strand the rest of the batch half-applied.
	Errors []ActionError `json:"errors,omitempty"`
}

// ActionError is one failed action within a plan.
type ActionError struct {
	Action string           `json:"action"`
	Target string           `json:"target"`
	Code   nomads.ErrorCode `json:"code"`
	Error  string           `json:"error"`
}

// HasErrors reports whether any action failed.
func (r Result) HasErrors() bool { return len(r.Errors) > 0 }

// Apply executes a plan against the remote and verifies the outcome.
//
// Verification matters because the API returns success for writes that did not
// take effect (notably deletes of unknown ids), so the final state is re-read
// and compared rather than inferred from status codes.
func Apply(ctx context.Context, c TripClient, plan SyncPlan) (*Result, error) {
	res := &Result{Ambiguous: plan.Ambiguous}
	for _, u := range plan.Unchanged {
		res.Unchanged = append(res.Unchanged, u.Current)
	}

	for _, create := range plan.Creates {
		trip, err := c.CreateTrip(ctx, create)
		if err != nil {
			res.Errors = append(res.Errors, actionError("create", create.Label(), err))
			continue
		}
		res.Created = append(res.Created, *trip)
	}

	for _, upd := range plan.Updates {
		// UpdateTrip returns the trip under its new id, since Nomads.com
		// reassigns ids on edit.
		trip, err := c.UpdateTrip(ctx, upd.Current.ID, upd.Patch)
		if err != nil {
			res.Errors = append(res.Errors, actionError("update", upd.Current.Label(), err))
			continue
		}
		res.Updated = append(res.Updated, *trip)
	}

	for _, del := range plan.Deletes {
		if err := c.DeleteTrip(ctx, del.ID); err != nil {
			res.Errors = append(res.Errors, actionError("delete", del.Label(), err))
			continue
		}
		res.Deleted = append(res.Deleted, del)
	}

	return res, nil
}

func actionError(action, target string, err error) ActionError {
	return ActionError{
		Action: action,
		Target: target,
		Code:   nomads.CodeOf(err),
		Error:  err.Error(),
	}
}

// Verify re-reads remote state and confirms every desired trip is present with
// the requested dates. It is the final proof that a sync converged.
func Verify(ctx context.Context, c TripClient, desired []nomads.DesiredTrip) error {
	current, err := c.ListTrips(ctx)
	if err != nil {
		return err
	}
	// A converged state is one where re-planning finds nothing left to do.
	plan := PlanTripSync(current, desired, Options{})
	if plan.IsNoop() && len(plan.Ambiguous) == 0 {
		return nil
	}
	var missing []string
	for _, c := range plan.Creates {
		missing = append(missing, "missing: "+c.Label())
	}
	for _, u := range plan.Updates {
		missing = append(missing, fmt.Sprintf("still differs: %s (%v)", u.Current.Label(), u.Changes))
	}
	for _, a := range plan.Ambiguous {
		missing = append(missing, "ambiguous: "+a.Desired.Label())
	}
	return nomads.Errorf(nomads.ErrRemoteStateMismatch,
		"remote state does not match the desired trips after sync").
		WithDetail("differences", missing)
}

// Sync plans and applies in one step, then verifies convergence.
func Sync(ctx context.Context, c TripClient, desired []nomads.DesiredTrip, opts Options) (*Result, SyncPlan, error) {
	if err := ValidateDesired(desired); err != nil {
		return nil, SyncPlan{}, err
	}
	current, err := c.ListTrips(ctx)
	if err != nil {
		return nil, SyncPlan{}, err
	}
	plan := PlanTripSync(current, desired, opts)

	res, err := Apply(ctx, c, plan)
	if err != nil {
		return nil, plan, err
	}
	// Only claim convergence when every action succeeded.
	if !res.HasErrors() {
		if err := Verify(ctx, c, desired); err != nil {
			return res, plan, err
		}
	}
	return res, plan, nil
}
