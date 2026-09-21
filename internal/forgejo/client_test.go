package forgejo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestV16Contract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token secret" {
			t.Error("missing authentication")
		}
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/jobs"):
			if r.URL.Query().Get("labels") != "linux,x64" {
				t.Error("labels not encoded")
			}
			w.Write([]byte(`[{"id":7,"handle":"attempt-2","status":"waiting","runs_on":["linux","x64"]}]`))
		case r.Method == "POST":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["ephemeral"] != true {
				t.Error("runner must be ephemeral")
			}
			w.WriteHeader(201)
			w.Write([]byte(`{"id":4,"uuid":"uuid","token":"runner-secret"}`))
		case r.Method == "DELETE":
			w.WriteHeader(404)
		default:
			t.Error("unexpected request", r.URL)
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	c, err := New(server.URL+"/subpath", "secret", "Repository", "owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := c.Jobs(context.Background(), []string{"linux", "x64"})
	if err != nil || len(jobs) != 1 || jobs[0].Handle != "attempt-2" {
		t.Fatalf("jobs: %v %v", jobs, err)
	}
	runner, err := c.Register(context.Background(), "worker", "owner")
	if err != nil || runner.ID != 4 {
		t.Fatalf("register: %v", err)
	}
	if err = c.Delete(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
}

func TestLabelMatching(t *testing.T) {
	if !Matches([]string{"linux", "x64"}, []string{"linux", "x64", "extra"}) {
		t.Fatal("subset must match")
	}
	if Matches([]string{"linux", "arm64"}, []string{"linux", "x64"}) {
		t.Fatal("missing label must not match")
	}
	if Matches(nil, []string{"linux"}) {
		t.Fatal("empty runs-on must not match")
	}
}

func TestErrorsDoNotLeakResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403); w.Write([]byte("secret-token")) }))
	defer server.Close()
	c, _ := New(server.URL, "secret", "Repository", "owner/repo")
	_, err := c.Jobs(context.Background(), []string{"linux"})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestUnfilteredJobsOmitsLabelsParameter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("labels") {
			t.Error("empty labels filter hides labelled jobs in v16")
		}
		w.Write([]byte(`null`))
	}))
	defer server.Close()
	c, _ := New(server.URL, "token", "Repository", "owner/repo")
	jobs, err := c.Jobs(context.Background(), nil)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("empty queue: %v %v", jobs, err)
	}
}

func TestRunnerPaginationHandlesServerPageCap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("visible") != "false" {
			t.Error("must only list runners directly owned by scope")
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Write([]byte(`[{"id":1}]`))
		case "2":
			w.Write([]byte(`[{"id":2}]`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer server.Close()
	c, _ := New(server.URL, "token", "Repository", "owner/repo")
	runners, err := c.Runners(context.Background())
	if err != nil || len(runners) != 2 {
		t.Fatalf("pagination dropped runners: %v %v", runners, err)
	}
}

func TestScopeRoutes(t *testing.T) {
	for _, test := range []struct{ kind, name, want string }{
		{"Repository", "owner/repo", "/repos/owner/repo/actions/runners"},
		{"Organization", "team", "/orgs/team/actions/runners"},
		{"User", "", "/user/actions/runners"},
		{"Instance", "", "/admin/actions/runners"},
	} {
		c, err := New("https://forgejo.example/subpath/", "token", test.kind, test.name)
		if err != nil || c.route != test.want || c.base != "https://forgejo.example/subpath/api/v1" {
			t.Fatalf("%s: %v %v", test.kind, c, err)
		}
	}
	for _, test := range []struct{ kind, name string }{{"User", "someone"}, {"Instance", "global"}, {"Repository", "../repo"}, {"Organization", "a/b"}, {"Unknown", ""}} {
		if _, err := New("https://forgejo.example", "token", test.kind, test.name); err == nil {
			t.Fatalf("invalid scope accepted: %v", test)
		}
	}
}
