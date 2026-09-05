// handlers_deploy.go owns the `deploy/` subtree: CRUD over the triggers an
// operator has authorised, and the preflight that proves a deploy would work
// before one is attempted.
//
// EVERY ROUTE HERE REQUIRES ScopeDeploy, reads included. Trigger config names
// repositories, branches and allowed scopes — the same admin-grade metadata
// that made listing PATs require `actions` rather than `read`. There is no
// read-only corner of this subtree.
//
// What is NOT here: firing a deploy. That lands with the executor, and keeping
// it out until then is deliberate — an endpoint that accepts a fire before
// anything validates a commit's ancestry is an endpoint somebody can call.

package controller

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"go.voodu.clowk.in/internal/activity"
	"time"
)

// maxTriggers bounds how many a single box will hold.
//
// Not a resource limit — a hundred records is nothing. It is a shape check: a
// box accumulating triggers past this is one where something is creating them
// in a loop, and discovering that at ten thousand is worse than being refused
// at a hundred.
const maxTriggers = 100

// handleDeployTriggers routes the collection: list and create.
func (a *API) handleDeployTriggers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.listTriggers(w, r)
	case http.MethodPost:
		a.createTrigger(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))
	}
}

// handleDeployTrigger routes one record: read, replace, delete.
func (a *API) handleDeployTrigger(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("trigger id is required"))

		return
	}

	switch r.Method {
	case http.MethodGet:
		a.getTrigger(w, r, id)
	case http.MethodPut:
		a.updateTrigger(w, r, id)
	case http.MethodDelete:
		a.deleteTrigger(w, r, id)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeErr(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed", r.Method))
	}
}

func (a *API) listTriggers(w http.ResponseWriter, r *http.Request) {
	triggers, err := a.Store.ListTriggers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	// Never `null` on the wire: a client doing `.triggers.length` on a box
	// that has none should see 0, not a type error.
	if triggers == nil {
		triggers = []Trigger{}
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: map[string]any{"triggers": triggers}})
}

func (a *API) getTrigger(w http.ResponseWriter, r *http.Request, id string) {
	trigger, err := a.Store.GetTrigger(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	if trigger == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no trigger %q", id))

		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: trigger})
}

func (a *API) createTrigger(w http.ResponseWriter, r *http.Request) {
	input, ok := decodeTriggerInput(w, r)
	if !ok {
		return
	}

	existing, err := a.Store.ListTriggers(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	if len(existing) >= maxTriggers {
		writeErr(w, http.StatusConflict,
			fmt.Errorf("this box already holds %d triggers; delete one before adding another", len(existing)))

		return
	}

	// One trigger per (repo, branch). A second for the same pair is not a
	// second authorisation — it is the same statement written twice, and two
	// records that must agree is a way for them to disagree.
	for i := range existing {
		if existing[i].MatchesRepo(input.Repo) && existing[i].Branch == input.Branch {
			writeErr(w, http.StatusConflict,
				fmt.Errorf("trigger %s already authorises %s on %s — update it instead",
					existing[i].ID, existing[i].Repo, existing[i].Branch))

			return
		}
	}

	trigger := Trigger{
		ID:          NewTriggerID(),
		Repo:        input.Repo,
		Branch:      input.Branch,
		AllowScopes: input.AllowScopes,
		Enabled:     input.Enabled == nil || *input.Enabled,
		CreatedAt:   time.Now().UTC(),
	}

	if err := a.Store.PutTrigger(r.Context(), trigger); err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	a.recordTriggerChange(r, activity.ActionTriggerCreate, trigger)

	writeJSON(w, http.StatusCreated, envelope{Status: "ok", Data: trigger})
}

// updateTrigger REPLACES the mutable fields of an existing record.
//
// Replace and not merge: this is a statement of trust, and a partial update
// leaves the operator guessing which half of it they just changed. `Enabled`
// is the exception — omitting it keeps the current value, because pausing and
// re-authorising are different intentions and one should not silently perform
// the other.
func (a *API) updateTrigger(w http.ResponseWriter, r *http.Request, id string) {
	current, err := a.Store.GetTrigger(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	if current == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no trigger %q", id))

		return
	}

	input, ok := decodeTriggerInput(w, r)
	if !ok {
		return
	}

	updated := *current
	updated.Repo = input.Repo
	updated.Branch = input.Branch
	updated.AllowScopes = input.AllowScopes

	if input.Enabled != nil {
		updated.Enabled = *input.Enabled
	}

	if err := a.Store.PutTrigger(r.Context(), updated); err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	a.recordTriggerChange(r, activity.ActionTriggerUpdate, updated)

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: updated})
}

func (a *API) deleteTrigger(w http.ResponseWriter, r *http.Request, id string) {
	// Read before deleting, so the trail can say WHAT was withdrawn. A line
	// carrying only an id would record that trust was removed without
	// recording from what — which is the half that matters.
	previous, _ := a.Store.GetTrigger(r.Context(), id)

	deleted, err := a.Store.DeleteTrigger(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	if !deleted {
		writeErr(w, http.StatusNotFound, fmt.Errorf("no trigger %q", id))

		return
	}

	if previous != nil {
		a.recordTriggerChange(r, activity.ActionTriggerDelete, *previous)
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: map[string]any{"deleted": id}})
}

// decodeTriggerInput reads and normalises the body, writing the error response
// itself so each caller is one `if !ok { return }` rather than four.
func decodeTriggerInput(w http.ResponseWriter, r *http.Request) (TriggerInput, bool) {
	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return TriggerInput{}, false
	}

	var input TriggerInput
	if err := json.Unmarshal(body, &input); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))

		return TriggerInput{}, false
	}

	normalized, err := input.Normalize()
	if err != nil {
		var invalid ErrTriggerInvalid

		if errors.As(err, &invalid) {
			writeErr(w, http.StatusBadRequest, err)

			return TriggerInput{}, false
		}

		writeErr(w, http.StatusInternalServerError, err)

		return TriggerInput{}, false
	}

	return normalized, true
}

// NewTriggerID returns the handle the control plane fires against.
//
// Random, and NOT derived from the repository name: a repository can be
// renamed on GitHub without anything telling this box, and an id that changed
// underneath a configured control plane would break deploys in a way nobody
// could trace back to a rename.
func NewTriggerID() string {
	var b [8]byte

	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is close to impossible. A timestamp still
		// yields a usable handle, and returning "" would produce a record
		// that cannot be addressed.
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000")))
	}

	return hex.EncodeToString(b[:])
}

// recordTriggerChange writes the trust statement's new shape to the trail.
//
// WHY THIS EXISTS AT ALL. The console may create and edit triggers over the
// PAT plane — a deliberate decision, because the alternative is customers
// handing SSH keys to their whole team. It trades "a control plane cannot
// widen what this box accepts" for "it can, and the owner can see it". These
// lines are the second half of that trade; without them it is not a trade, it
// is just the loss.
//
// `done` and not a pair: writing a trigger is one store round trip, and an
// action that cannot be observed mid-flight should not claim a duration.
func (a *API) recordTriggerChange(r *http.Request, action activity.Action, trigger Trigger) {
	a.recordActivity(r, activity.Record{
		Event:  activity.EventDone,
		Action: action,
		Name:   trigger.Repo,
		Status: activity.StatusSucceeded,
		Trigger: &activity.TriggerSnapshot{
			ID:          trigger.ID,
			Repo:        trigger.Repo,
			Branch:      trigger.Branch,
			AllowScopes: trigger.AllowScopes,
			Enabled:     trigger.Enabled,
		},
	})
}
