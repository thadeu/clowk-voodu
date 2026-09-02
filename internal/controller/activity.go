package controller

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.voodu.clowk.in/internal/activity"
	"go.voodu.clowk.in/internal/clientinfo"
)

// recordActivity appends one line to the action trail.
//
// THE seam. Every instrumented handler calls this and nothing else, so the
// rules that must hold everywhere — never fail the action, fill origin and
// actor from the request, default the timestamp — are stated once.
//
// A nil Writer is a supported state, not a bug: test setups don't wire one, and
// a box whose disk refused the writer still has to operate. Errors are logged
// and swallowed for the same reason.
func (a *API) recordActivity(r *http.Request, rec activity.Record) {
	if a.Activity == nil {
		return
	}

	if rec.ID == "" {
		rec.ID = NewActivityID()
	}

	if rec.Ts.IsZero() {
		rec.Ts = time.Now().UTC()
	}

	if r != nil {
		if rec.Origin == "" {
			rec.Origin = activity.NormalizeOrigin(r.Header.Get(activity.OriginHeader))
		}

		// The actor is the VERIFIED PAT id from the auth middleware, never a
		// caller-supplied header. Absent on the internal plane, where a CLI on
		// the box itself has no PAT — an empty actor there is a fact of the
		// design, not a gap.
		if rec.Actor == "" {
			if id, ok := PATIDFromContext(r.Context()); ok {
				rec.Actor = id
			}
		}
	}

	// The two client-declared facts. Read here, in the one seam every
	// instrumented handler goes through, so a handler added later carries them
	// without its author knowing they exist.
	if r != nil {
		if rec.Client == nil {
			rec.Client = decodeClientInfo(r.Header.Get(clientinfo.Header))
		}

		if len(rec.Files) == 0 {
			rec.Files = decodeFiles(r.Header.Get(filesHeader))
		}
	}

	if err := a.Activity.Write(rec); err != nil {
		log.Printf("activity: write %s/%s: %v", rec.Action, rec.Event, err)
	}
}

// NewActivityID returns the id correlating a `started` line with its
// `finished` one.
//
// Random rather than sequential on purpose: two actions can be in flight at
// once, and a counter would need state that survives a restart to stay unique.
// 16 hex chars is plenty for a 30-day window holding hundreds of rows.
func NewActivityID() string {
	var b [8]byte

	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is close to impossible; a timestamp-derived id
		// is still unique enough to correlate a pair, and returning "" would
		// silently break correlation for every row after the failure.
		return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000")))
	}

	return hex.EncodeToString(b[:])
}

// elapsedMS is the millisecond duration recorded on a finished/done line.
// Floors at zero so a clock adjustment can't write a negative duration.
func elapsedMS(start time.Time) int64 {
	d := time.Since(start).Milliseconds()

	if d < 0 {
		return 0
	}

	return d
}

// activityTracker records a pair of lines around one action.
//
// WHY A WRAPPER AND NOT A LINE PER RETURN. applyPost alone has a dozen
// `writeErr(...); return` paths. Recording the outcome at each one means a
// future path added without the call silently stops being audited — the
// failure mode of an audit trail that is worst, because the gap is invisible.
//
// So the tracker wraps the ResponseWriter, reads the STATUS CODE the handler
// actually wrote, and fires from a defer. Every exit is covered, including a
// panic, and a new error path is audited without anyone remembering to.
type activityTracker struct {
	api *API
	r   *http.Request

	// Record is enriched in place as the handler learns what it is doing —
	// scope, name, resources are only known part-way through.
	Record activity.Record

	start  time.Time
	status *activityStatusWriter
	paired bool
	fired  bool
}

// beginActivity starts tracking an action, returning the writer the handler
// must use from here on plus the tracker.
//
// `paired` chooses the shape: true writes `started` now and `finished` at the
// end (a long action — apply, restart), false writes a single `done` at the end
// (an instantaneous one — delete, rollback, config). Three event values exist
// precisely so an instantaneous action is not forced to lie about having a
// duration.
//
// Returns w unchanged when no writer is wired, so a test setup pays nothing.
func (a *API) beginActivity(w http.ResponseWriter, r *http.Request, paired bool, rec activity.Record) (http.ResponseWriter, *activityTracker) {
	if a.Activity == nil {
		return w, &activityTracker{fired: true}
	}

	if rec.ID == "" {
		rec.ID = NewActivityID()
	}

	t := &activityTracker{
		api:    a,
		r:      r,
		Record: rec,
		start:  time.Now(),
		status: &activityStatusWriter{ResponseWriter: w, status: http.StatusOK},
		paired: paired,
	}

	if paired {
		started := rec
		started.Event = activity.EventStarted
		a.recordActivity(r, started)
	}

	return t.status, t
}

// Finish writes the closing line. Idempotent, and safe on a tracker from a
// controller with no writer.
//
// Call it deferred, immediately after beginActivity, so an early return or a
// panic still closes the pair. A `started` with no `finished` is not a
// catastrophe — it reads as "in flight, or the controller died mid-action" —
// but it should mean that and nothing else.
func (t *activityTracker) Finish() {
	if t == nil || t.fired {
		return
	}

	t.fired = true

	rec := t.Record

	if t.paired {
		rec.Event = activity.EventFinished
	} else {
		rec.Event = activity.EventDone
	}

	rec.Ts = time.Now().UTC()
	rec.ElapsedMS = elapsedMS(t.start)

	// The HTTP status the handler actually wrote is the honest outcome. A
	// handler that set Status itself (a partial apply) keeps its answer:
	// `partial` is a distinction 200 cannot make.
	if rec.Status == "" {
		if t.status.status >= 400 {
			rec.Status = activity.StatusFailed
		} else {
			rec.Status = activity.StatusSucceeded
		}
	}

	if rec.Error == "" && t.status.status >= 400 {
		rec.Error = t.status.errorText()
	}

	t.api.recordActivity(t.r, rec)
}

// maxActivityErrorBytes caps what is captured from an error response. Enough
// for the message writeErr produced, never enough for a body that turned out
// to carry something large.
const maxActivityErrorBytes = 512

// activityStatusWriter observes the status code and, on an error, the first
// bytes of the body — so the trail carries WHY an action failed, not just that
// it did.
//
// Buffers nothing on the success path: a 200 body can be a streamed log or a
// large manifest list, and none of it belongs in an audit line.
type activityStatusWriter struct {
	http.ResponseWriter

	status int
	body   []byte
}

func (s *activityStatusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *activityStatusWriter) Write(b []byte) (int, error) {
	if s.status >= 400 && len(s.body) < maxActivityErrorBytes {
		room := maxActivityErrorBytes - len(s.body)

		if room > len(b) {
			room = len(b)
		}

		s.body = append(s.body, b[:room]...)
	}

	return s.ResponseWriter.Write(b)
}

// Flush forwards to the wrapped writer. Without it, wrapping a streaming
// handler would silently buffer the response — the bug is invisible until a
// log tail stops arriving live.
func (s *activityStatusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// errorText pulls the message out of the error envelope writeErr produced,
// falling back to the raw bytes when the body is not that shape.
func (s *activityStatusWriter) errorText() string {
	if len(s.body) == 0 {
		return ""
	}

	var env struct {
		Error string `json:"error"`
	}

	if err := json.Unmarshal(s.body, &env); err == nil && env.Error != "" {
		return env.Error
	}

	return strings.TrimSpace(string(s.body))
}

// manifestResources renders a batch as the trail's resource list.
//
// Kind/scope/name only — never the spec. A manifest can carry an entire
// deployment definition, and the trail is a timeline of what happened, not a
// second copy of the store. Diffing two applies would need the spec, and that
// is deliberately a different feature.
func manifestResources(ms []*Manifest) []activity.Resource {
	if len(ms) == 0 {
		return nil
	}

	out := make([]activity.Resource, 0, len(ms))

	for _, m := range ms {
		if m == nil {
			continue
		}

		out = append(out, activity.Resource{
			Kind:  string(m.Kind),
			Scope: m.Scope,
			Name:  m.Name,
		})
	}

	return out
}

// recordConfigChange writes ONE LINE PER COMMAND for a config change.
//
// One line and not one per key, which is what it did first. `vd config set
// A=1 B=2 C=3` is a single thing the operator did; three rows made a reader
// recognise them as one command by squinting at a shared id, and the screen
// counted three changes where one happened. The keys are a list ON the row,
// and the screen expands it into a table.
//
// THE VALUES ARE NEVER WRITTEN — only each key and a short digest. This file
// has 30-day retention, is served over the PAT plane and is mirrored into the
// WebUI's SQLite warehouse; recording values would create a second plaintext
// copy of every production secret, with its own retention and its own read
// surface, routing around every protection the config bucket has.
//
// A delete passes empty values; there is nothing to digest, so no digest is
// written rather than the hash of "".
func (a *API) recordConfigChange(r *http.Request, action activity.Action, scope, name string, payload map[string]string, cause error) {
	if a.Activity == nil || len(payload) == 0 {
		return
	}

	// Sorted, because Go randomises map iteration and an audit line whose
	// field order changes between two identical commands is a line nobody can
	// diff.
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	changes := make([]activity.ConfigChange, 0, len(keys))

	for _, key := range keys {
		change := activity.ConfigChange{Key: key}

		if value := payload[key]; value != "" {
			change.ValueDigest = activity.DigestValue(value)
		}

		changes = append(changes, change)
	}

	rec := activity.Record{
		ID:         NewActivityID(),
		Ts:         time.Now().UTC(),
		Event:      activity.EventDone,
		Action:     action,
		Scope:      scope,
		Name:       name,
		ConfigKeys: changes,
		Status:     activity.StatusSucceeded,
	}

	if cause != nil {
		rec.Status = activity.StatusFailed
		rec.Error = cause.Error()
	}

	a.recordActivity(r, rec)
}

// commonScope returns the scope every manifest in the batch shares, or "" when
// they differ.
//
// Empty on a mixed batch rather than the first one found: a row claiming a
// scope that only some of its resources belong to would make the scope filter
// answer wrongly in the other direction, and no answer is better than a wrong
// one for a filter people use to decide where to look.
func commonScope(ms []*Manifest) string {
	scope := ""
	seen := false

	for _, m := range ms {
		if m == nil {
			continue
		}

		if !seen {
			scope = m.Scope
			seen = true

			continue
		}

		if m.Scope != scope {
			return ""
		}
	}

	return scope
}

// filesHeader carries the operator's original `-f` arguments. Mirrors the
// constant on the CLI side; duplicated rather than imported because cmd/cli is
// a main package and cannot be depended on.
const filesHeader = "X-Voodu-Files"

// maxRecordedFiles bounds what a client can put in the trail. The CLI already
// caps its own list; this is the server-side half of the same limit, because a
// header is whatever the caller sends and the trail is append-only — an
// unbounded list would be a way to grow the file on purpose.
const maxRecordedFiles = 20

// maxFilePathBytes bounds one entry, for the same reason.
const maxFilePathBytes = 256

// decodeClientInfo turns the client header into the trail's shape.
//
// Returns nil for anything absent or malformed. A caller that sends garbage
// gets no attribution, never an error: this is a nicety on the side of an
// action, and the action must not fail because the nicety was unreadable.
func decodeClientInfo(raw string) *activity.ClientInfo {
	info := clientinfo.Decode(raw)
	if info.Empty() {
		return nil
	}

	return &activity.ClientInfo{
		IP:      info.IP,
		City:    info.City,
		Region:  info.Region,
		Country: info.Country,
		Org:     info.Org,
	}
}

// decodeFiles unpacks the base64 comma-joined `-f` list, bounded on both count
// and length. Base64 because a path can hold spaces and commas, and the value
// travels as a shell word before it becomes a header.
func decodeFiles(raw string) []string {
	if raw == "" {
		return nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}

	var out []string

	for _, part := range strings.Split(string(decoded), ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}

		if len(p) > maxFilePathBytes {
			p = p[:maxFilePathBytes]
		}

		out = append(out, p)

		if len(out) >= maxRecordedFiles {
			break
		}
	}

	return out
}

// configDeleteKeys reads the keys a DELETE /config names.
//
// `keys=A,B,C` is what a multi-key unset sends; `key=A` is the single-key form
// every CLI has always sent and still does. Reading both keeps a one-key unset
// working across any version pairing, and confines the skew risk to the
// multi-key case where an older controller fails loudly instead of quietly
// deleting only the first.
//
// Blank entries are dropped so `keys=A,,B` is two keys, not three.
func configDeleteKeys(r *http.Request) []string {
	raw := r.URL.Query().Get("keys")
	if raw == "" {
		raw = r.URL.Query().Get("key")
	}

	var out []string

	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}

	return out
}
