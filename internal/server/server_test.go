package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ota-takeru/glab-mr-graph/internal/graph"
	"github.com/ota-takeru/glab-mr-graph/internal/oteltrace"
)

type serverTraceRecord struct {
	name  string
	start oteltrace.Attributes
	end   oteltrace.Attributes
	err   error
}

type serverRecordingTracer struct {
	mu    sync.Mutex
	spans []*serverTraceRecord
}

type serverRecordingSpan struct {
	tracer *serverRecordingTracer
	span   *serverTraceRecord
}

func (t *serverRecordingTracer) Start(ctx context.Context, name string, _ oteltrace.SpanKind, attributes oteltrace.Attributes) (context.Context, oteltrace.Span) {
	record := &serverTraceRecord{name: name, start: cloneServerTraceAttributes(attributes)}
	t.mu.Lock()
	t.spans = append(t.spans, record)
	t.mu.Unlock()
	return ctx, &serverRecordingSpan{tracer: t, span: record}
}

func (s *serverRecordingSpan) End(err error, attributes oteltrace.Attributes) {
	s.tracer.mu.Lock()
	s.span.err = err
	s.span.end = cloneServerTraceAttributes(attributes)
	s.tracer.mu.Unlock()
}

func cloneServerTraceAttributes(attributes oteltrace.Attributes) oteltrace.Attributes {
	result := make(oteltrace.Attributes, len(attributes))
	for key, value := range attributes {
		result[key] = value
	}
	return result
}

type fakeLoader struct{ options graph.SearchOptions }

type progressLoader struct{ fakeLoader }

type stagedProgressLoader struct{ progressLoader }

type flushRecorder struct {
	*httptest.ResponseRecorder
	snapshots []string
}

func (r *flushRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.snapshots = append(r.snapshots, r.Body.String())
}

func (f *stagedProgressLoader) LoadStages(_ context.Context, _ graph.SearchOptions, progress func(int, int, string, int), emit func(string, graph.Result)) (graph.Result, error) {
	progress(1, 1, "Discovering stacked merge requests", 1)
	topology := graph.Result{Nodes: []graph.Node{{ID: "topology-node", Kind: "pullRequest"}}}
	complete := graph.Result{Nodes: []graph.Node{{ID: "complete-node", Kind: "pullRequest"}}}
	emit("topology", topology)
	emit("complete", complete)
	return complete, nil
}

func (f *progressLoader) LoadProgress(_ context.Context, _ graph.SearchOptions, progress func(int, int, string, int)) (graph.Result, error) {
	progress(1, 1, "Searching merge requests", 1)
	progress(1, 1, "Discovering stacked merge requests", 1)
	return graph.Result{UpdatedAt: time.Unix(1, 0)}, nil
}

func (f *progressLoader) LoadIncluded(_ context.Context, prs []*graph.PullRequest, progress func(int, int, string)) ([]graph.IncludedUpdate, error) {
	progress(len(prs), len(prs), "Inspecting merge request details")
	return []graph.IncludedUpdate{{PullRequestID: "pr1", IncludedPullRequests: []graph.IncludedPullRequest{{ID: "included1", Number: 1}}}}, nil
}

func (f *progressLoader) InspectPullRequest(_ context.Context, pr *graph.PullRequest) (graph.IncludedUpdate, error) {
	return graph.IncludedUpdate{PullRequestID: pr.ID, IncludedPullRequests: []graph.IncludedPullRequest{{Number: 42}}, Truncated: true}, nil
}

func (f *fakeLoader) Load(_ context.Context, options graph.SearchOptions) (graph.Result, error) {
	f.options = options
	return graph.Result{UpdatedAt: time.Unix(1, 0)}, nil
}

func TestGraphHandler(t *testing.T) {
	loader := &fakeLoader{}
	s := New(loader)
	recorder := httptest.NewRecorder()
	s.graph(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/graph?q=is%3Aopen", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if loader.options.Query != "is:open" || !loader.options.Authored || !loader.options.Assigned || !loader.options.ReviewRequested {
		t.Fatalf("options = %+v", loader.options)
	}
	var result graph.Result
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
}

func TestServerTraceDoesNotRecordPrivateRequestValues(t *testing.T) {
	const secretQuery = "private-search-value"
	const secretHost = "private.gitlab.example"
	const secretID = "gitlab:mr:987"
	tracer := &serverRecordingTracer{}
	s := New(&fakeLoader{})
	s.SetTracer(tracer)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/graph?q="+secretQuery+"&branch=private-branch", nil)
	request.Host = secretHost
	s.graph(httptest.NewRecorder(), request)

	tracer.mu.Lock()
	records := append([]*serverTraceRecord(nil), tracer.spans...)
	tracer.mu.Unlock()
	if len(records) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(records))
	}
	for _, record := range records {
		encoded := fmt.Sprintf("%s %v %v %v", record.name, record.start, record.end, record.err)
		for _, secret := range []string{secretQuery, secretHost, secretID, "private-branch"} {
			if strings.Contains(encoded, secret) {
				t.Errorf("trace contains private value %q: %s", secret, encoded)
			}
		}
		if got := record.start["pr.has_search_query"]; got != true {
			t.Errorf("pr.has_search_query = %#v, want true", got)
		}
	}
}

func TestServerTraceDoesNotRecordPullRequestID(t *testing.T) {
	const secretID = "gitlab:mr:987"
	tracer := &serverRecordingTracer{}
	s := New(&progressLoader{})
	s.SetTracer(tracer)
	inspectBody := strings.NewReader(`{"id":"` + secretID + `"}`)
	s.inspect(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/inspect", inspectBody))
	includedBody := strings.NewReader(`{"pullRequests":[{"id":"` + secretID + `","includedPRs":[{"id":"private-included"}]}]}`)
	s.included(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/included", includedBody))

	tracer.mu.Lock()
	records := append([]*serverTraceRecord(nil), tracer.spans...)
	tracer.mu.Unlock()
	if len(records) != 2 {
		t.Fatalf("recorded spans = %d, want 2", len(records))
	}
	for _, record := range records {
		encoded := fmt.Sprintf("%s %v %v", record.name, record.start, record.end)
		if strings.Contains(encoded, secretID) || strings.Contains(encoded, "private-included") {
			t.Errorf("trace contains pull-request identity: %s", encoded)
		}
	}
}

func TestMetaReturnsConfiguredVersion(t *testing.T) {
	s := New(&fakeLoader{})
	s.SetVersion("v1.2.3")
	recorder := httptest.NewRecorder()
	s.meta(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil))
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Body.String(); !strings.Contains(got, `"version":"v1.2.3"`) {
		t.Errorf("body = %s", got)
	}
}

func TestGraphHandlerReadsExplicitScopes(t *testing.T) {
	loader := &fakeLoader{}
	s := New(loader)
	recorder := httptest.NewRecorder()
	s.graph(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/graph?authored=1&assigned=0&reviewRequested=0", nil))
	if !loader.options.Authored || loader.options.Assigned || loader.options.ReviewRequested {
		t.Fatalf("options = %+v", loader.options)
	}
}

func TestSecurityHeaders(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := recorder.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("Content-Security-Policy is missing")
	}
}

func TestShouldFallbackPort(t *testing.T) {
	if !shouldFallbackPort(fmt.Errorf("listen: %w", syscall.EADDRINUSE)) {
		t.Fatal("address-in-use error should fall back")
	}
	if shouldFallbackPort(errors.New("unrelated")) {
		t.Fatal("unrelated error should not fall back")
	}
}

func TestProgressPercent(t *testing.T) {
	tests := []struct {
		current, total int
		phase          string
		want           int
	}{
		{1, 2, "Searching merge requests", 10},
		{1, 2, "Discovering stacked merge requests", 42},
		{1, 2, "Inspecting merge request details", 82},
		{1, 2, "Fetching included pull requests", 95},
		{1, 1, "Complete", 100},
	}
	for _, tt := range tests {
		if got := progressPercent(tt.current, tt.total, tt.phase); got != tt.want {
			t.Errorf("progressPercent(%d, %d, %q) = %d, want %d", tt.current, tt.total, tt.phase, got, tt.want)
		}
	}
}

func TestGraphStreamsProgressAndResult(t *testing.T) {
	s := New(&progressLoader{})
	recorder := httptest.NewRecorder()
	s.graph(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/graph", nil))
	body := recorder.Body.String()
	if !strings.Contains(body, `"percent":80`) || !strings.Contains(body, `"phase":"Building merge request graph"`) || !strings.Contains(body, `"collected":0`) || !strings.Contains(body, `"type":"result"`) {
		t.Fatalf("unexpected stream: %s", body)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/x-ndjson") {
		t.Fatalf("content type = %q", got)
	}
}

func TestGraphStreamsTopologyBeforeComplete(t *testing.T) {
	s := New(&stagedProgressLoader{})
	recorder := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.graph(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/graph", nil))
	lines := strings.Split(strings.TrimSpace(recorder.Body.String()), "\n")
	stages := make([]string, 0, 2)
	for _, line := range lines {
		var event struct {
			Type  string `json:"type"`
			Stage string `json:"stage"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "result" {
			stages = append(stages, event.Stage)
		}
	}
	if fmt.Sprint(stages) != "[topology complete]" {
		t.Fatalf("result stages = %v, want topology then complete", stages)
	}
	topologyFlushedBeforeComplete := false
	for _, snapshot := range recorder.snapshots {
		if strings.Contains(snapshot, `"stage":"topology"`) && !strings.Contains(snapshot, `"stage":"complete"`) {
			topologyFlushedBeforeComplete = true
			break
		}
	}
	if !topologyFlushedBeforeComplete {
		t.Fatalf("topology was not flushed before complete: %v", recorder.snapshots)
	}
}

func TestInspectReturnsIncludedCandidates(t *testing.T) {
	s := New(&progressLoader{})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/inspect", strings.NewReader(`{"id":"pr1","number":99}`))
	s.inspect(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"number":42`) || !strings.Contains(recorder.Body.String(), `"includedPullRequestsTruncated":true`) {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestIncludedStreamsProgressAndUpdates(t *testing.T) {
	s := New(&progressLoader{})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/included", strings.NewReader(`{"pullRequests":[{"id":"pr1"}]}`))
	s.included(recorder, request)
	body := recorder.Body.String()
	if !strings.Contains(body, `"percent":100`) || !strings.Contains(body, `"pullRequestId":"pr1"`) || !strings.Contains(body, `"type":"result"`) {
		t.Fatalf("unexpected stream: %s", body)
	}
}

func TestIncludedRequiresOneContainingPullRequest(t *testing.T) {
	s := New(&progressLoader{})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/included", strings.NewReader(`{"pullRequests":[]}`))
	s.included(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
