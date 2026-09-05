package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.voodu.clowk.in/internal/activity"
)

func newDeployAPI(t *testing.T) (*API, *httptest.Server) {
	t.Helper()

	api, _ := newTestAPI(t)
	ts := httptest.NewServer(api.Handler())

	t.Cleanup(ts.Close)

	return api, ts
}

func createTriggerVia(t *testing.T, ts *httptest.Server, body string) (*http.Response, Trigger) {
	t.Helper()

	resp := postBody(t, ts.URL+"/deploy/triggers", body)

	defer resp.Body.Close()

	var env struct {
		Data Trigger `json:"data"`
	}

	if resp.StatusCode == http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatal(err)
		}
	}

	return resp, env.Data
}

func TestCreateTriggerStoresTheTrustStatement(t *testing.T) {
	_, ts := newDeployAPI(t)

	resp, trigger := createTriggerVia(t, ts,
		`{"repo":"Acme/Web","branch":"main","allow_scopes":["runa","runa"]}`)

	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}

	if trigger.ID == "" {
		t.Fatal("no id assigned")
	}

	if trigger.Repo != "acme/web" {
		t.Errorf("repo = %q, want the canonical lowercase form", trigger.Repo)
	}

	if len(trigger.AllowScopes) != 1 {
		t.Errorf("scopes = %v, want deduped", trigger.AllowScopes)
	}

	// A trigger created without saying otherwise is on: the operator just
	// authorised it, and creating something disabled would be a surprise.
	if !trigger.Enabled {
		t.Error("a new trigger should be enabled")
	}

	if trigger.CreatedAt.IsZero() {
		t.Error("created_at was not stamped")
	}
}

// Fields the SERVER owns must be unreachable from a request body — not merely
// ignored by convention, but absent from the type a body decodes into.
func TestCreateTriggerIgnoresServerOwnedFields(t *testing.T) {
	_, ts := newDeployAPI(t)

	_, trigger := createTriggerVia(t, ts,
		`{"repo":"acme/web","branch":"main","allow_scopes":["runa"],
		  "id":"attacker-chosen","created_at":"2001-01-01T00:00:00Z"}`)

	if trigger.ID == "attacker-chosen" {
		t.Fatal("the request body chose the trigger id")
	}

	if trigger.CreatedAt.Year() == 2001 {
		t.Fatal("the request body chose created_at")
	}
}

// The same statement written twice is two records that must agree, which is a
// way for them to disagree.
func TestCreateTriggerRefusesADuplicate(t *testing.T) {
	_, ts := newDeployAPI(t)

	createTriggerVia(t, ts, `{"repo":"acme/web","branch":"main","allow_scopes":["runa"]}`)

	// Different spelling, same statement.
	resp, _ := createTriggerVia(t, ts, `{"repo":"ACME/Web","branch":"main","allow_scopes":["other"]}`)

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}

	// A different BRANCH of the same repository is a different statement.
	ok, _ := createTriggerVia(t, ts, `{"repo":"acme/web","branch":"staging","allow_scopes":["runa"]}`)

	if ok.StatusCode != http.StatusCreated {
		t.Fatalf("a second branch should be allowed, got %d", ok.StatusCode)
	}
}

func TestCreateTriggerRefusesAnEmptyAllowList(t *testing.T) {
	_, ts := newDeployAPI(t)

	resp, _ := createTriggerVia(t, ts, `{"repo":"acme/web","branch":"main","allow_scopes":[]}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — a trigger allowing nothing is a mistake, not a policy", resp.StatusCode)
	}
}

func TestListAndGetTrigger(t *testing.T) {
	_, ts := newDeployAPI(t)

	_, created := createTriggerVia(t, ts, `{"repo":"acme/web","branch":"main","allow_scopes":["runa"]}`)

	resp, err := http.Get(ts.URL + "/deploy/triggers")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	var listed struct {
		Data struct {
			Triggers []Trigger `json:"triggers"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}

	if len(listed.Data.Triggers) != 1 {
		t.Fatalf("listed %d triggers", len(listed.Data.Triggers))
	}

	one, err := http.Get(ts.URL + "/deploy/triggers/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}

	defer one.Body.Close()

	if one.StatusCode != http.StatusOK {
		t.Fatalf("get status %d", one.StatusCode)
	}

	missing, err := http.Get(ts.URL + "/deploy/triggers/nope")
	if err != nil {
		t.Fatal(err)
	}

	defer missing.Body.Close()

	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id status %d, want 404", missing.StatusCode)
	}
}

// An empty box answers with an empty list, never `null`: a client doing
// `.triggers.length` should see 0, not a type error.
func TestListTriggersIsNeverNull(t *testing.T) {
	_, ts := newDeployAPI(t)

	resp, err := http.Get(ts.URL + "/deploy/triggers")
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), "null") {
		t.Fatalf("empty list rendered as null: %s", raw)
	}
}

// Pausing and re-authorising are different intentions. Omitting `enabled`
// keeps the current value so one cannot silently perform the other.
func TestUpdateTriggerKeepsEnabledWhenOmitted(t *testing.T) {
	_, ts := newDeployAPI(t)

	_, created := createTriggerVia(t, ts, `{"repo":"acme/web","branch":"main","allow_scopes":["runa"]}`)

	disabled := putJSON(t, ts.URL+"/deploy/triggers/"+created.ID,
		`{"repo":"acme/web","branch":"main","allow_scopes":["runa"],"enabled":false}`)

	if disabled.Enabled {
		t.Fatal("enabled:false was not applied")
	}

	// Now change the scopes without mentioning `enabled`.
	kept := putJSON(t, ts.URL+"/deploy/triggers/"+created.ID,
		`{"repo":"acme/web","branch":"main","allow_scopes":["runa","staging"]}`)

	if kept.Enabled {
		t.Fatal("omitting enabled silently re-enabled a paused trigger")
	}

	if len(kept.AllowScopes) != 2 {
		t.Errorf("scopes = %v", kept.AllowScopes)
	}
}

func TestDeleteTriggerIsIdempotentlyReported(t *testing.T) {
	_, ts := newDeployAPI(t)

	_, created := createTriggerVia(t, ts, `{"repo":"acme/web","branch":"main","allow_scopes":["runa"]}`)

	first := deleteAt(t, ts.URL+"/deploy/triggers/"+created.ID)
	if first != http.StatusOK {
		t.Fatalf("first delete: %d", first)
	}

	second := deleteAt(t, ts.URL+"/deploy/triggers/"+created.ID)
	if second != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", second)
	}
}

func putJSON(t *testing.T, url, body string) Trigger {
	t.Helper()

	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put status %d", resp.StatusCode)
	}

	var env struct {
		Data Trigger `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}

	return env.Data
}

func deleteAt(t *testing.T, url string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	defer resp.Body.Close()

	return resp.StatusCode
}

// THE acceptance criterion of the deploy subtree: every route under it needs
// ScopeDeploy, and no other scope substitutes — reads included.
//
// Trigger config names repositories, branches and allowed scopes, which is the
// same admin-grade metadata that made listing PATs require `actions` rather
// than `read`. A monitoring integration holding `read` must not be able to
// enumerate what this box deploys.
func TestDeployRoutesRequireScopeDeploy(t *testing.T) {
	store := newMemStore()
	auth := newPATAuthorizer(store, quietLogger())

	tokens := map[Scope]string{}

	for _, s := range []Scope{ScopeRead, ScopeActions, ScopeConfig, ScopeDeploy} {
		plain, _ := seedTestPAT(t, store, []Scope{s})
		tokens[s] = plain
	}

	routes := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/pat/v1/deploy/triggers"},
		{http.MethodPost, "/api/pat/v1/deploy/triggers"},
		{http.MethodGet, "/api/pat/v1/deploy/triggers/abc"},
		{http.MethodPut, "/api/pat/v1/deploy/triggers/abc"},
		{http.MethodDelete, "/api/pat/v1/deploy/triggers/abc"},
	}

	for _, route := range routes {
		for scope, plain := range tokens {
			name := route.method + " " + route.path + " with " + string(scope)

			t.Run(name, func(t *testing.T) {
				req := httptest.NewRequest(route.method, route.path, nil)
				signRequest(req, plain)

				rr := httptest.NewRecorder()
				called := false

				auth.Middleware(ScopeDeploy, nextOK(t, &called))(rr, req)

				if scope == ScopeDeploy {
					if rr.Code != http.StatusOK {
						t.Errorf("deploy scope refused: %d (%s)", rr.Code, rr.Body.String())
					}

					return
				}

				if rr.Code != http.StatusForbidden {
					t.Errorf("scope %q reached a deploy route: %d", scope, rr.Code)
				}

				if called {
					t.Errorf("scope %q was let through to the handler", scope)
				}
			})
		}
	}
}

// The console may create and edit triggers over the PAT plane — a deliberate
// decision, because the alternative is customers handing SSH keys to their
// whole team. It trades "a control plane cannot widen what this box accepts"
// for "it can, and the owner can see it".
//
// These lines are the second half of that trade. Without them it is not a
// trade, it is just the loss — the owner would have no way to see what the
// control plane authorised in their name.
func TestTriggerChangesLandInTheActivityTrail(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	_, created := createTriggerVia(t, ts, `{"repo":"acme/web","branch":"main","allow_scopes":["runa"]}`)

	putJSON(t, ts.URL+"/deploy/triggers/"+created.ID,
		`{"repo":"acme/web","branch":"main","allow_scopes":["runa","staging"]}`)

	deleteAt(t, ts.URL+"/deploy/triggers/"+created.ID)

	recs := trail(t, dir)

	if len(recs) != 3 {
		t.Fatalf("want create, update and delete recorded, got %d: %+v", len(recs), recs)
	}

	wantActions := []activity.Action{
		activity.ActionTriggerCreate,
		activity.ActionTriggerUpdate,
		activity.ActionTriggerDelete,
	}

	for i, want := range wantActions {
		if recs[i].Action != want {
			t.Errorf("line %d = %q, want %q", i, recs[i].Action, want)
		}

		if recs[i].Trigger == nil {
			t.Fatalf("line %d carries no trigger — a line that records trust changed without recording to what", i)
		}

		if recs[i].Trigger.Repo != "acme/web" {
			t.Errorf("line %d repo = %q", i, recs[i].Trigger.Repo)
		}
	}

	// The AFTER state, not a diff: an operator reading the trail wants "what
	// does this box now accept", and reconstructing that from a chain of diffs
	// is work the reader should not have to do.
	if len(recs[1].Trigger.AllowScopes) != 2 {
		t.Errorf("the update should record the widened scope list: %v", recs[1].Trigger.AllowScopes)
	}

	// The delete records what was WITHDRAWN. A line carrying only an id would
	// say trust was removed without saying from what.
	if len(recs[2].Trigger.AllowScopes) != 2 || recs[2].Trigger.Branch != "main" {
		t.Errorf("the delete should record the trigger as it stood: %+v", recs[2].Trigger)
	}
}

// The trail is where the owner sees what the console did, so it must say the
// console did it.
func TestTriggerChangesRecordTheOrigin(t *testing.T) {
	api, _, dir := newActivityAPI(t)
	ts := httptest.NewServer(api.Handler())

	defer ts.Close()

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/deploy/triggers",
		strings.NewReader(`{"repo":"acme/web","branch":"main","allow_scopes":["runa"]}`))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(activity.OriginHeader, string(activity.OriginDeployPlane))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()

	recs := trail(t, dir)

	if len(recs) != 1 || recs[0].Origin != activity.OriginDeployPlane {
		t.Fatalf("origin = %q, want deploy_plane: %+v", recs[0].Origin, recs)
	}
}
