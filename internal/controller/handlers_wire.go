// handlers_wire.go serves `vd wire`: the host's identity and peers, and
// adding or removing a peer. Every change is applied to wg0 before the
// response and lands in the activity trail.

package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.voodu.clowk.in/internal/activity"
)

func (a *API) wireOr503(w http.ResponseWriter) *Wire {
	if a.Wire == nil {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("wire: this controller has no WireGuard support"))

		return nil
	}

	return a.Wire
}

// handleWireStatus answers GET /wire: identity + peers with their link.
func (a *API) handleWireStatus(w http.ResponseWriter, r *http.Request) {
	wire := a.wireOr503(w)
	if wire == nil {
		return
	}

	id, peers, err := wire.Status(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)

		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   map[string]any{"identity": id, "peers": peers},
	})
}

// handleWirePeerAdd answers POST /wire/peers with a WirePeer body.
func (a *API) handleWirePeerAdd(w http.ResponseWriter, r *http.Request) {
	wire := a.wireOr503(w)
	if wire == nil {
		return
	}

	body, err := readBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	var p WirePeer

	if err := json.Unmarshal(body, &p); err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))

		return
	}

	w, act := a.beginActivity(w, r, false, activity.Record{
		Action: activity.ActionWireAdd,
		Name:   strings.TrimSpace(p.Address),
	})

	defer act.Finish()

	added, err := wire.Add(r.Context(), p)
	if err != nil {
		writeErr(w, wireErrorCode(err), err)

		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: map[string]any{"peer": added}})
}

// handleWirePeerRemove answers DELETE /wire/peers/{address}.
func (a *API) handleWirePeerRemove(w http.ResponseWriter, r *http.Request) {
	wire := a.wireOr503(w)
	if wire == nil {
		return
	}

	address := strings.TrimSpace(r.PathValue("address"))

	w, act := a.beginActivity(w, r, false, activity.Record{
		Action: activity.ActionWireRemove,
		Name:   address,
	})

	defer act.Finish()

	if err := wire.Remove(r.Context(), address); err != nil {
		writeErr(w, wireErrorCode(err), err)

		return
	}

	writeJSON(w, http.StatusOK, envelope{Status: "ok", Data: map[string]any{"removed": address}})
}

// wireErrorCode: what the operator typed is 400, what they asked for and
// does not exist is 404, and the store or wg failing is 503.
func wireErrorCode(err error) int {
	switch {
	case errors.Is(err, ErrWirePeerNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrWireUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadRequest
	}
}

// handleWireUFW answers GET /wire/ufw: the ufw rules this host needs, for
// `vd wire ufw` to print or apply. The controller only computes them — its
// sandbox cannot write /etc/ufw, and should not.
func (a *API) handleWireUFW(w http.ResponseWriter, r *http.Request) {
	wire := a.wireOr503(w)
	if wire == nil {
		return
	}

	bridge, rules, err := wire.UFW()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)

		return
	}

	writeJSON(w, http.StatusOK, envelope{
		Status: "ok",
		Data:   map[string]any{"bridge": bridge, "rules": rules},
	})
}
