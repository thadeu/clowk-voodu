// handlers_activity.go owns the two read paths over the operator-action
// trail. Same split as metrics, for the same reason:
//
//   - `GET /activity`      — a filtered, bounded, newest-first list. The
//     screen path.
//   - `GET /activity/dump` — every line since a timestamp, raw. The
//     warehouse-sync path.
//
// One on-disk store, two read paths, no coupling between them.

package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.voodu.clowk.in/internal/activity"
)

// maxActivityLimit caps `?limit`. A screen asking for more than this is
// asking for a dump, and the dump endpoint is the one built for that.
const maxActivityLimit = 1000

// activityListResponse is the envelope of GET /activity.
//
// An envelope rather than a bare array, so `count` and future paging fields
// have somewhere to live without breaking every client — the same call the
// metrics endpoint made.
type activityListResponse struct {
	Records []activity.Record `json:"records"`
	Count   int               `json:"count"`
}

// handleActivity answers the filtered query.
//
//	GET /activity?action=apply&origin=cli&status=failed&scope=&kind=&name=&limit=
//
// Records come back NEWEST FIRST, which is both how a history screen reads and
// what makes `limit` mean "the most recent N" instead of "the oldest N of
// thirty days".
//
// Repeatable filters accept a comma-separated list (`?action=apply,restart`) —
// one round trip for "show me the changes", rather than one per action.
//
// 503 when ActivityDir isn't wired (test setups). 400 on a malformed param: a
// caller who mistyped a status wants to hear about it, not to silently receive
// an unfiltered list.
func (a *API) handleActivity(w http.ResponseWriter, r *http.Request) {
	if a.ActivityDir == "" {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("activity store not configured"))

		return
	}

	q := r.URL.Query()

	opts := activity.QueryOpts{
		Dir:   a.ActivityDir,
		Scope: q.Get("scope"),
		Kind:  q.Get("kind"),
		Name:  q.Get("name"),
	}

	limit, err := parseActivityLimit(q.Get("limit"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	opts.Limit = limit

	since, err := parseSince(q.Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	opts.Start = since

	until, err := parseSince(q.Get("until"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	opts.End = until

	for _, v := range csvParam(q.Get("action")) {
		opts.Actions = append(opts.Actions, activity.Action(v))
	}

	// Origin goes through NormalizeOrigin, so `?origin=nonsense` filters for
	// `api` rather than matching nothing — consistent with how an unknown
	// origin is recorded in the first place.
	for _, v := range csvParam(q.Get("origin")) {
		opts.Origins = append(opts.Origins, activity.NormalizeOrigin(v))
	}

	for _, v := range csvParam(q.Get("status")) {
		st := activity.Status(v)

		switch st {
		case activity.StatusSucceeded, activity.StatusFailed, activity.StatusPartial:
			opts.Statuses = append(opts.Statuses, st)
		default:
			writeErr(w, http.StatusBadRequest, fmt.Errorf("status: unknown value %q", v))

			return
		}
	}

	records, err := activity.Query(opts)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	// Never `null` on the wire: a client doing `.records.length` on an empty
	// history should see 0, not a type error.
	if records == nil {
		records = []activity.Record{}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	_ = json.NewEncoder(w).Encode(activityListResponse{Records: records, Count: len(records)})
}

// handleActivityDump streams raw NDJSON lines newer than `since`.
//
//	GET /activity/dump?since=<unix_ts>
//
// `application/x-ndjson`, one object per line, chunked — byte-for-byte the
// contract `/metrics/dump` has, so the WebUI's sync job is the same shape of
// code against both stores.
//
// Lines are written verbatim from disk. A field added to Record reaches the
// warehouse without a change here.
func (a *API) handleActivityDump(w http.ResponseWriter, r *http.Request) {
	if a.ActivityDir == "" {
		writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("activity store not configured"))

		return
	}

	since, err := parseSince(r.URL.Query().Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	w.WriteHeader(http.StatusOK)

	// Headers are flushed by here, so an error mid-stream cannot become a
	// status code. The client sees a truncated stream and retries with the
	// same `since` on the next tick — nothing is lost.
	_ = activity.Dump(w, activity.DumpOpts{Dir: a.ActivityDir, Since: since})
}

// parseActivityLimit bounds `?limit`. Empty → 0, which lets the reader apply
// its own default rather than duplicating the number here.
func parseActivityLimit(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit: %w", err)
	}

	if n < 0 {
		return 0, fmt.Errorf("limit must be non-negative")
	}

	if n > maxActivityLimit {
		n = maxActivityLimit
	}

	return n, nil
}

// csvParam splits a repeatable filter, dropping empties so `?action=apply,`
// is not a filter for the empty action.
func csvParam(raw string) []string {
	if raw == "" {
		return nil
	}

	var out []string

	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}

	return out
}
