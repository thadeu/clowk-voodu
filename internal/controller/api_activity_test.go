package controller

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/activity"
	"go.voodu.clowk.in/internal/clientinfo"
)

// newActivityAPI is newTestAPI with a real trail wired to a temp dir.
func newActivityAPI(t *testing.T) (*API, *memStore, string) {
	t.Helper()

	api, store := newTestAPI(t)

	dir := t.TempDir()

	w, err := activity.NewWriter(dir, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { w.Close() })

	api.Activity = w
	api.ActivityDir = dir

	return api, store, dir
}

// trail reads every record written so far, oldest first.
func trail(t *testing.T, dir string) []activity.Record {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var out []activity.Record

	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}

		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			if len(line) == 0 {
				continue
			}

			var rec activity.Record

			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatalf("bad line %q: %v", line, err)
			}

			out = append(out, rec)
		}
	}

	return out
}

func TestApplyRecordsStartedAndFinished(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	body := `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"x:1"}}`

	resp := postBody(t, ts.URL+"/apply", body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	recs := trail(t, dir)

	if len(recs) != 2 {
		t.Fatalf("want started+finished, got %d: %+v", len(recs), recs)
	}

	if recs[0].Event != activity.EventStarted || recs[1].Event != activity.EventFinished {
		t.Fatalf("events: %q, %q", recs[0].Event, recs[1].Event)
	}

	// The pair is only useful if it correlates.
	if recs[0].ID != recs[1].ID || recs[0].ID == "" {
		t.Fatalf("ids do not correlate: %q vs %q", recs[0].ID, recs[1].ID)
	}

	if recs[1].Status != activity.StatusSucceeded {
		t.Errorf("status = %q", recs[1].Status)
	}

	if recs[1].Name != "api" || recs[1].Scope != "test" {
		t.Errorf("resource: scope=%q name=%q", recs[1].Scope, recs[1].Name)
	}
}

// The reason the tracker wraps the ResponseWriter instead of recording at each
// return: an error path must close the pair without anyone remembering to.
func TestApplyRecordsFailureWithTheErrorMessage(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	// A kind with no plugin installed. It fails in expandPluginBlocks —
	// AFTER beginActivity, which is the path this test is about: the
	// handler returns from a writeErr nobody instrumented, and the pair
	// still closes.
	resp := postBody(t, ts.URL+"/apply", `{"kind":"postgres","scope":"test","name":"db","spec":{}}`)
	resp.Body.Close()

	if resp.StatusCode < 400 {
		t.Fatalf("expected the apply to fail, got %d", resp.StatusCode)
	}

	recs := trail(t, dir)

	if len(recs) != 2 {
		t.Fatalf("want started+finished, got %d", len(recs))
	}

	if recs[1].Status != activity.StatusFailed {
		t.Fatalf("status = %q, want failed", recs[1].Status)
	}

	if recs[1].Error == "" {
		t.Error("the failure reason was not captured")
	}
}

// dry_run is `vd diff` — a read. A row for it would claim something happened
// to the box when nothing did.
func TestDryRunApplyIsNotRecorded(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	resp := postBody(t, ts.URL+"/apply?dry_run=true", `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"x:1"}}`)
	resp.Body.Close()

	if got := trail(t, dir); len(got) != 0 {
		t.Fatalf("dry run wrote %d rows: %+v", len(got), got)
	}
}

func TestConfigSetRecordsOneLinePerCommandAndNeverTheValue(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	const secret = "postgres://user:hunter2@db/prod"

	resp := postBody(t, ts.URL+"/config?scope=acme&name=web&restart=false",
		`{"DATABASE_URL":"`+secret+`","NODE_ENV":"production"}`)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	recs := trail(t, dir)

	// ONE line: `vd config set A=1 B=2` is a single thing the operator did.
	// It used to be one per key, and the screen counted three changes where
	// one happened.
	if len(recs) != 1 {
		t.Fatalf("want one line for one command, got %d: %+v", len(recs), recs)
	}

	rec := recs[0]

	if rec.Event != activity.EventDone || rec.Action != activity.ActionConfigSet {
		t.Errorf("event=%q action=%q", rec.Event, rec.Action)
	}

	if len(rec.ConfigKeys) != 2 {
		t.Fatalf("keys = %+v", rec.ConfigKeys)
	}

	// Sorted, because Go randomises map iteration and a line whose field order
	// changes between two identical commands is a line nobody can diff.
	if rec.ConfigKeys[0].Key != "DATABASE_URL" || rec.ConfigKeys[1].Key != "NODE_ENV" {
		t.Errorf("keys are not sorted: %+v", rec.ConfigKeys)
	}

	for _, change := range rec.ConfigKeys {
		if change.ValueDigest == "" {
			t.Errorf("%s: no digest", change.Key)
		}
	}

	// THE invariant. If this ever fails, the trail became a second plaintext
	// copy of every production secret.
	raw := rawTrailBytes(t, dir)

	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("hunter2")) {
		t.Fatal("a config value reached the activity file")
	}
}

func TestConfigDeleteRecordsEveryKeyInOneLineWithoutDigests(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	postBody(t, ts.URL+"/config?scope=acme&name=web&restart=false",
		`{"FOO":"bar","BAR":"baz"}`).Body.Close()

	// `keys` is the multi-key form; a single-key unset still sends `key`.
	req, err := http.NewRequest(http.MethodDelete,
		ts.URL+"/config?scope=acme&name=web&keys=FOO,BAR&restart=false", nil)
	if err != nil {
		t.Fatal(err)
	}

	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	del.Body.Close()

	if del.StatusCode != http.StatusOK {
		t.Fatalf("delete status %d", del.StatusCode)
	}

	var found *activity.Record

	for _, rec := range trail(t, dir) {
		if rec.Action == activity.ActionConfigDelete {
			r := rec
			found = &r
		}
	}

	if found == nil {
		t.Fatal("no config.delete row")
	}

	if len(found.ConfigKeys) != 2 {
		t.Fatalf("want both keys on one line, got %+v", found.ConfigKeys)
	}

	// Nothing was set, so there is nothing to digest — hashing "" would be a
	// value that looks meaningful and is not.
	for _, change := range found.ConfigKeys {
		if change.ValueDigest != "" {
			t.Errorf("%s carried a digest: %q", change.Key, change.ValueDigest)
		}
	}
}

// The single-key form every CLI has always sent still works, so a one-key
// unset keeps working against any controller version.
func TestConfigDeleteStillAcceptsTheSingularKeyParam(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	postBody(t, ts.URL+"/config?scope=acme&name=web&restart=false", `{"FOO":"bar"}`).Body.Close()

	req, err := http.NewRequest(http.MethodDelete,
		ts.URL+"/config?scope=acme&name=web&key=FOO&restart=false", nil)
	if err != nil {
		t.Fatal(err)
	}

	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	del.Body.Close()

	if del.StatusCode != http.StatusOK {
		t.Fatalf("status %d", del.StatusCode)
	}

	for _, rec := range trail(t, dir) {
		if rec.Action != activity.ActionConfigDelete {
			continue
		}

		if len(rec.ConfigKeys) != 1 || rec.ConfigKeys[0].Key != "FOO" {
			t.Fatalf("keys = %+v", rec.ConfigKeys)
		}

		return
	}

	t.Fatal("no config.delete row")
}

// The origin the client declares must land on the row; the controller cannot
// work it out on its own.
func TestOriginHeaderIsRecorded(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/apply",
		strings.NewReader(`{"kind":"deployment","scope":"test","name":"api","spec":{"image":"x:1"}}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(activity.OriginHeader, "receive_pack")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	recs := trail(t, dir)

	if len(recs) == 0 {
		t.Fatal("nothing recorded")
	}

	for _, rec := range recs {
		if rec.Origin != activity.OriginReceivePack {
			t.Errorf("origin = %q, want receive_pack", rec.Origin)
		}
	}
}

// A controller with no writer must keep working. This is what makes it safe to
// say "recording never fails an action".
func TestActionsWorkWithNoActivityWriter(t *testing.T) {
	api, store := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	resp := postBody(t, ts.URL+"/apply", `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"x:1"}}`)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	if got, _ := store.Get(t.Context(), KindDeployment, "test", "api"); got == nil {
		t.Fatal("apply did not store the manifest")
	}
}

func TestActivityEndpointsReadBackWhatWasWritten(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	postBody(t, ts.URL+"/apply", `{"kind":"deployment","scope":"test","name":"api","spec":{"image":"x:1"}}`).Body.Close()
	postBody(t, ts.URL+"/config?scope=acme&name=web&restart=false", `{"FOO":"bar"}`).Body.Close()

	resp, err := http.Get(ts.URL + "/activity")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var listed struct {
		Records []activity.Record `json:"records"`
		Count   int               `json:"count"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}

	if listed.Count != len(trail(t, dir)) {
		t.Fatalf("listed %d, on disk %d", listed.Count, len(trail(t, dir)))
	}

	// Newest first is the contract a history screen depends on.
	if len(listed.Records) > 1 && listed.Records[0].Ts.Before(listed.Records[1].Ts) {
		t.Error("records are not newest-first")
	}

	// Filtering is the reason the endpoint exists rather than dump alone.
	filtered, err := http.Get(ts.URL + "/activity?action=config.set")
	if err != nil {
		t.Fatal(err)
	}

	defer filtered.Body.Close()

	var only struct {
		Records []activity.Record `json:"records"`
	}

	if err := json.NewDecoder(filtered.Body).Decode(&only); err != nil {
		t.Fatal(err)
	}

	if len(only.Records) == 0 {
		t.Fatal("action filter matched nothing")
	}

	for _, rec := range only.Records {
		if rec.Action != activity.ActionConfigSet {
			t.Errorf("filter leaked a %q row", rec.Action)
		}
	}
}

func TestActivityDumpStreamsNDJSON(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	postBody(t, ts.URL+"/config?scope=acme&name=web&restart=false", `{"FOO":"bar"}`).Body.Close()

	resp, err := http.Get(ts.URL + "/activity/dump")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-ndjson") {
		t.Errorf("content-type = %q", ct)
	}

	got := new(bytes.Buffer)

	if _, err := got.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}

	// Verbatim: the dump must be the bytes on disk, so a field added to
	// Record reaches the warehouse without a controller change.
	if !bytes.Equal(bytes.TrimSpace(got.Bytes()), bytes.TrimSpace(rawTrailBytes(t, dir))) {
		t.Errorf("dump is not the on-disk bytes:\n got: %s\nwant: %s", got.Bytes(), rawTrailBytes(t, dir))
	}
}

func rawTrailBytes(t *testing.T, dir string) []byte {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	out := new(bytes.Buffer)

	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}

		out.Write(raw)
	}

	return out.Bytes()
}

// The gap this closes: an apply of five resources that all live in one scope
// left the scope column empty, so the action did not appear under a filter
// every one of its resources matched.
func TestApplyRecordsTheScopeSharedByTheWholeBatch(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	body := `[
	  {"kind":"deployment","scope":"runa","name":"web","spec":{"image":"x:1"}},
	  {"kind":"deployment","scope":"runa","name":"worker","spec":{"image":"x:1"}}
	]`

	resp := postBody(t, ts.URL+"/apply", body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	recs := trail(t, dir)
	last := recs[len(recs)-1]

	if last.Scope != "runa" {
		t.Fatalf("scope = %q, want runa", last.Scope)
	}

	// No single name for a batch — that is what Resources is for.
	if last.Name != "" {
		t.Errorf("name = %q, want empty for a multi-resource apply", last.Name)
	}

	if len(last.Resources) != 2 {
		t.Errorf("resources = %+v", last.Resources)
	}
}

// A batch spanning two scopes has no scope. Naming one of them would make the
// filter answer wrongly for the other, and no answer beats a wrong one.
func TestApplyLeavesScopeEmptyWhenTheBatchSpansScopes(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	body := `[
	  {"kind":"deployment","scope":"runa","name":"web","spec":{"image":"x:1"}},
	  {"kind":"deployment","scope":"other","name":"web","spec":{"image":"x:1"}}
	]`

	resp := postBody(t, ts.URL+"/apply", body)
	resp.Body.Close()

	recs := trail(t, dir)
	last := recs[len(recs)-1]

	if last.Scope != "" {
		t.Fatalf("scope = %q, want empty for a mixed batch", last.Scope)
	}
}

// The client facts arrive as headers because the controller cannot observe
// either one: its view of a CLI peer is always 127.0.0.1, and the file names
// were replaced with `-f -` before the command left the operator's machine.
func TestApplyRecordsTheClientFactsFromHeaders(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	info := clientinfo.Info{
		IP: "189.4.22.10", City: "Sao Paulo", Region: "Sao Paulo",
		Country: "BR", Org: "AS28573 Claro S.A.",
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/apply",
		strings.NewReader(`{"kind":"deployment","scope":"runa","name":"api","spec":{"image":"x:1"}}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(activity.OriginHeader, "ssh")
	req.Header.Set(clientinfo.Header, info.Encode())
	req.Header.Set("X-Voodu-Files", base64.RawURLEncoding.EncodeToString([]byte("infra/db.hcl,infra/web.hcl")))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	recs := trail(t, dir)
	last := recs[len(recs)-1]

	if last.Client == nil {
		t.Fatal("no client info recorded")
	}

	if last.Client.IP != "189.4.22.10" || last.Client.City != "Sao Paulo" {
		t.Errorf("client = %+v", last.Client)
	}

	if len(last.Files) != 2 || last.Files[0] != "infra/db.hcl" {
		t.Errorf("files = %v", last.Files)
	}
}

// A header is whatever the caller sends, and this file is append-only. An
// unbounded list would be a way to grow it on purpose.
func TestRecordedFilesAreBounded(t *testing.T) {
	many := make([]string, 100)
	for i := range many {
		many[i] = strings.Repeat("x", 400)
	}

	got := decodeFiles(base64.RawURLEncoding.EncodeToString([]byte(strings.Join(many, ","))))

	if len(got) != maxRecordedFiles {
		t.Fatalf("kept %d entries, want %d", len(got), maxRecordedFiles)
	}

	if len(got[0]) != maxFilePathBytes {
		t.Errorf("entry length %d, want %d", len(got[0]), maxFilePathBytes)
	}
}

// Both headers cross two hops. A mangled one must leave the action alone.
func TestMalformedClientHeadersAreIgnored(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/apply",
		strings.NewReader(`{"kind":"deployment","scope":"runa","name":"api","spec":{"image":"x:1"}}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(clientinfo.Header, "!!! not base64 !!!")
	req.Header.Set("X-Voodu-Files", "!!! not base64 !!!")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a bad header failed the apply: %d", resp.StatusCode)
	}

	last := trail(t, dir)[1]

	if last.Client != nil || len(last.Files) != 0 {
		t.Errorf("garbage was recorded: client=%+v files=%v", last.Client, last.Files)
	}
}
