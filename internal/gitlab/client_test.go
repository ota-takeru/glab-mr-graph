package gitlab

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ota-takeru/glab-mr-graph/internal/graph"
	"github.com/ota-takeru/glab-mr-graph/internal/oteltrace"
)

type clientTraceRecord struct {
	name  string
	start oteltrace.Attributes
	end   oteltrace.Attributes
	err   error
}

type clientRecordingTracer struct {
	mu    sync.Mutex
	spans []*clientTraceRecord
}

type clientRecordingSpan struct {
	tracer *clientRecordingTracer
	span   *clientTraceRecord
}

func (t *clientRecordingTracer) Start(ctx context.Context, name string, _ oteltrace.SpanKind, attributes oteltrace.Attributes) (context.Context, oteltrace.Span) {
	record := &clientTraceRecord{name: name, start: cloneTraceAttributes(attributes)}
	t.mu.Lock()
	t.spans = append(t.spans, record)
	t.mu.Unlock()
	return ctx, &clientRecordingSpan{tracer: t, span: record}
}

func (s *clientRecordingSpan) End(err error, attributes oteltrace.Attributes) {
	s.tracer.mu.Lock()
	s.span.err = err
	s.span.end = cloneTraceAttributes(attributes)
	s.tracer.mu.Unlock()
}

func cloneTraceAttributes(attributes oteltrace.Attributes) oteltrace.Attributes {
	result := make(oteltrace.Attributes, len(attributes))
	for key, value := range attributes {
		result[key] = value
	}
	return result
}

func TestBuildSearchSpecsUsesGitLabScopesAndFourthSearchBranch(t *testing.T) {
	specs := buildSearchSpecs(graph.SearchOptions{Authored: true, Assigned: true, ReviewRequested: true, Query: "database"})
	if len(specs) != 4 {
		t.Fatalf("spec count = %d, want 4", len(specs))
	}
	wantScopes := []string{"created_by_me", "assigned_to_me", "reviews_for_me", "all"}
	for i, want := range wantScopes {
		if specs[i].scope != want {
			t.Errorf("scope %d = %q, want %q", i, specs[i].scope, want)
		}
	}
	if specs[3].query != "database" {
		t.Errorf("search query = %q, want database", specs[3].query)
	}
}

func TestDecodeJSONPagesAcceptsMultipleJSONArrays(t *testing.T) {
	var got []rawMergeRequest
	if err := decodeJSONPages([]byte(`[{"id":1,"iid":7}]
[{"id":2,"iid":8}]`), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].IID != 8 {
		t.Fatalf("pages = %+v, want two merge requests", got)
	}
}

func TestAPICommandIncludesHostnameWithoutPaginate(t *testing.T) {
	var got []string
	c := New("gitlab.example.com")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		got = append([]string(nil), args...)
		return []byte(`[]`), nil
	})
	var response []rawMergeRequest
	if err := c.apiJSON(context.Background(), "fixture list", "/merge_requests?scope=all&search=title", &response); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "api") || strings.Contains(joined, "--paginate") || !strings.Contains(joined, "--hostname gitlab.example.com") {
		t.Fatalf("glab args = %v", got)
	}
	if got[len(got)-1] != "/merge_requests?scope=all&search=title" {
		t.Fatalf("endpoint = %q", got[len(got)-1])
	}
}

func TestClientTraceDoesNotRecordPrivateRequestValues(t *testing.T) {
	const secretQuery = "secret-query"
	const secretHost = "private.gitlab.example"
	const secretEndpoint = "/projects/987/merge_requests?target_branch=private-branch&search=" + secretQuery
	tracer := &clientRecordingTracer{}
	c := New(secretHost)
	c.Tracer = tracer
	c.Runner = RunnerFunc(func(_ context.Context, _ []string) ([]byte, error) {
		return []byte(`[]`), nil
	})
	var response []rawMergeRequest
	if err := c.apiJSON(context.Background(), "list merge requests", secretEndpoint, &response); err != nil {
		t.Fatal(err)
	}

	tracer.mu.Lock()
	records := append([]*clientTraceRecord(nil), tracer.spans...)
	tracer.mu.Unlock()
	if len(records) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(records))
	}
	for _, record := range records {
		encoded := fmt.Sprintf("%s %v %v %v", record.name, record.start, record.end, record.err)
		for _, secret := range []string{secretQuery, secretHost, "987", "private-branch", "merge_requests"} {
			if strings.Contains(encoded, secret) {
				t.Errorf("trace contains private value %q: %s", secret, encoded)
			}
		}
		if record.name != "glab api request" {
			t.Errorf("span name = %q, want fixed API span name", record.name)
		}
	}
}

func TestClientLoadTraceRecordsSearchPresenceOnly(t *testing.T) {
	const secret = "private-search-value"
	tracer := &clientRecordingTracer{}
	c := New("private.gitlab.example")
	c.Tracer = tracer
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		if args[len(args)-1] == "/user" {
			return []byte(`{"username":"viewer"}`), nil
		}
		return []byte(`[]`), nil
	})
	if _, err := c.Load(context.Background(), graph.SearchOptions{Query: secret}); err != nil {
		t.Fatal(err)
	}

	tracer.mu.Lock()
	records := append([]*clientTraceRecord(nil), tracer.spans...)
	tracer.mu.Unlock()
	seenRoot := false
	for _, record := range records {
		encoded := fmt.Sprintf("%s %v %v", record.name, record.start, record.end)
		if strings.Contains(encoded, secret) || strings.Contains(encoded, "private.gitlab.example") {
			t.Errorf("trace contains private search/host value: %s", encoded)
		}
		if record.name == "load merge request graph" {
			seenRoot = true
			if got := record.start["mr.has_search_query"]; got != true {
				t.Errorf("mr.has_search_query = %#v, want true", got)
			}
		}
	}
	if !seenRoot {
		t.Fatal("load root span was not recorded")
	}
}

func TestListAPIPaginatesManuallyAndStopsAtLimit(t *testing.T) {
	var endpoints []string
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		endpoints = append(endpoints, endpoint)
		return []byte(`[{"id":1,"iid":1},{"id":2,"iid":2},{"id":3,"iid":3}]`), nil
	})
	items, truncated, err := c.listAPI(context.Background(), "fixture list", "/merge_requests?state=opened", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ID != 1 || items[1].ID != 2 {
		t.Fatalf("items = %+v, want first two merge requests", items)
	}
	if !truncated {
		t.Fatal("list should report truncation when the limit is reached")
	}
	if len(endpoints) != 1 {
		t.Fatalf("page calls = %d, want one call after reaching the limit", len(endpoints))
	}
	if !strings.Contains(endpoints[0], "page=1") || !strings.Contains(endpoints[0], "per_page=100") {
		t.Fatalf("manual page endpoint = %q", endpoints[0])
	}
}

func TestListAPIDoesNotReportTruncationForExactShortPage(t *testing.T) {
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, _ []string) ([]byte, error) {
		return []byte(`[{"id":1,"iid":1},{"id":2,"iid":2}]`), nil
	})
	items, truncated, err := c.listAPI(context.Background(), "fixture list", "/merge_requests?state=opened", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || truncated {
		t.Fatalf("items/truncated = %d/%v, want 2/false for an exact short final page", len(items), truncated)
	}
}

func TestListAPIMovesToNextPageUntilShortPage(t *testing.T) {
	var pages []string
	fullPage := "[" + strings.TrimSuffix(strings.Repeat(`{"id":1,"iid":1},`, mergeRequestsPerPage), ",") + "]"
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		pages = append(pages, endpoint)
		if strings.HasPrefix(endpoint, "/merge_requests?page=1&") {
			return []byte(fullPage), nil
		}
		return []byte(`[]`), nil
	})
	items, truncated, err := c.listAPI(context.Background(), "fixture list", "/merge_requests?state=opened", 150)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != mergeRequestsPerPage || truncated {
		t.Fatalf("items/truncated = %d/%v, want %d items and false", len(items), truncated, mergeRequestsPerPage)
	}
	if len(pages) != 2 || !strings.Contains(pages[1], "page=2") {
		t.Fatalf("page endpoints = %v, want pages 1 and 2", pages)
	}
}

func TestConvertNormalizesGitLabStatusAndApprovalData(t *testing.T) {
	raw := rawMergeRequest{
		ID: 91, IID: 7, Title: "Improve query planner", WebURL: "https://gitlab.example.com/acme/db/-/merge_requests/7",
		Author: &rawUser{Username: "reviewer"}, Assignees: []rawUser{{Username: "me"}}, Reviewers: []rawUser{{Username: "me"}, {Username: "other"}},
		SourceProjectID: 11, TargetProjectID: 10, SourceBranch: "stack-2", TargetBranch: "stack-1",
		TargetProject: &rawProject{ID: 10, PathWithNamespace: "acme/db", WebURL: "https://gitlab.example.com/acme/db", DefaultBranch: "main"},
		SourceProject: &rawProject{ID: 11, PathWithNamespace: "acme/db", WebURL: "https://gitlab.example.com/acme/db", DefaultBranch: "main"},
		Draft:         true, DetailedMergeStatus: "conflict", HeadPipeline: &rawPipeline{Status: "running"},
	}
	approval := &rawApproval{ApprovalsRequired: 2, ApprovalsLeft: 1, ApprovedBy: []struct {
		User rawUser `json:"user"`
	}{{User: rawUser{Username: "alice"}}}}
	got := New("").convert(raw, approval, "me", "search")
	if got == nil {
		t.Fatal("convert returned nil")
	}
	if got.ID != "gitlab:mr:91" || got.Number != 7 || got.URL != raw.WebURL {
		t.Fatalf("identity = %q !%d %q", got.ID, got.Number, got.URL)
	}
	if !got.IsDraft || got.Mergeable != "CONFLICTING" || got.CIState != "PENDING" {
		t.Fatalf("state = draft=%v mergeable=%q ci=%q", got.IsDraft, got.Mergeable, got.CIState)
	}
	if got.ReviewApproved != 1 || got.ReviewTotal != 2 || got.Relation != "assigned" {
		t.Fatalf("review = %d/%d relation=%q", got.ReviewApproved, got.ReviewTotal, got.Relation)
	}
	if got.ApprovalState != "PENDING" || got.ApprovalRequired != 2 || got.ApprovalRemaining != 1 || got.ApproverCount != 1 {
		t.Fatalf("approval = %q required=%d remaining=%d approvers=%d", got.ApprovalState, got.ApprovalRequired, got.ApprovalRemaining, got.ApproverCount)
	}
}

func TestApprovalRuleStateDoesNotUseApprovedByCount(t *testing.T) {
	raw := rawMergeRequest{ID: 92, IID: 8, TargetProjectID: 10, SourceProjectID: 10, TargetBranch: "main", SourceBranch: "feature"}
	approval := &rawApproval{ApprovalsRequired: 2, ApprovalsLeft: 2, ApprovedBy: []struct {
		User rawUser `json:"user"`
	}{
		{User: rawUser{Username: "alice"}},
		{User: rawUser{Username: "bob"}},
	}}
	got := New("").convert(raw, approval, "viewer", "search")
	if got.ApprovalState != "PENDING" || got.ReviewDecision != "" || got.ApproverCount != 2 {
		t.Fatalf("approval = state=%q decision=%q approvers=%d, want pending despite two approved_by entries", got.ApprovalState, got.ReviewDecision, got.ApproverCount)
	}
}

func TestApprovalStateUsesApprovedFlagAndNoRules(t *testing.T) {
	raw := rawMergeRequest{ID: 93, IID: 9, TargetProjectID: 10, SourceProjectID: 10, TargetBranch: "main", SourceBranch: "feature", Reviewers: []rawUser{{Username: "reviewer"}}}
	approved := &rawApproval{ApprovalsRequired: 2, ApprovalsLeft: 2, Approved: true}
	got := New("").convert(raw, approved, "viewer", "search")
	if got.ApprovalState != "APPROVED" || got.ReviewDecision != "APPROVED" {
		t.Fatalf("approved state = %q decision=%q, want approved", got.ApprovalState, got.ReviewDecision)
	}
	noRules := &rawApproval{}
	got = New("").convert(raw, noRules, "viewer", "search")
	if got.ApprovalState != "NOT_REQUIRED" || got.ApprovalRequired != 0 || got.ApproverCount != 0 || got.ReviewTotal != 0 {
		t.Fatalf("no-rule approval = state=%q required=%d approvers=%d reviewTotal=%d", got.ApprovalState, got.ApprovalRequired, got.ApproverCount, got.ReviewTotal)
	}
}

func TestMergeStateOnlyReportsConfirmedConflicts(t *testing.T) {
	tests := []struct {
		name         string
		raw          rawMergeRequest
		wantConflict bool
	}{
		{name: "has conflicts", raw: rawMergeRequest{HasConflicts: true, DetailedMergeStatus: "checking"}, wantConflict: true},
		{name: "conflict status", raw: rawMergeRequest{DetailedMergeStatus: "conflict"}, wantConflict: true},
		{name: "unchecked", raw: rawMergeRequest{DetailedMergeStatus: "unchecked"}},
		{name: "cannot be merged", raw: rawMergeRequest{DetailedMergeStatus: "cannot_be_merged"}},
		{name: "need rebase", raw: rawMergeRequest{DetailedMergeStatus: "need_rebase"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeState(tt.raw) == "CONFLICTING"; got != tt.wantConflict {
				t.Fatalf("conflict = %v, want %v (state %q)", got, tt.wantConflict, mergeState(tt.raw))
			}
		})
	}
}

func TestBotDetectionUsesGitLabBotFlagOrExplicitSuffix(t *testing.T) {
	base := rawMergeRequest{ID: 1, IID: 1, TargetProjectID: 10, SourceProjectID: 10}
	tests := []struct {
		name   string
		author rawUser
		want   bool
	}{
		{name: "api bot flag", author: rawUser{Username: "release-service", Bot: true}, want: true},
		{name: "bot suffix", author: rawUser{Username: "dependabot[bot]"}, want: true},
		{name: "botanist is human", author: rawUser{Username: "botanist"}},
		{name: "robotics is human", author: rawUser{Username: "robotics"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := base
			raw.Author = &tt.author
			if got := New("").convert(raw, nil, "viewer", "search").IsBot; got != tt.want {
				t.Fatalf("IsBot = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSourceProjectFallbackKeepsForkIdentity(t *testing.T) {
	raw := rawMergeRequest{ID: 2, IID: 2, TargetProjectID: 10, SourceProjectID: 11, SourceBranch: "topic", TargetBranch: "main"}
	got := New("").convert(raw, nil, "viewer", "search")
	if got.HeadRepositoryID != "11" || got.HeadRepository != "project/11" {
		t.Fatalf("source fallback = %q/%q, want 11/project/11", got.HeadRepositoryID, got.HeadRepository)
	}
}

func TestBranchDiscoveryUsesGlobalUpstreamAndProjectScopedDownstream(t *testing.T) {
	var endpoint string
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint = args[len(args)-1]
		return []byte(`[]`), nil
	})
	if _, truncated, err := c.listBranchMergeRequests(context.Background(), branchSearch{projectID: "10", sourceBranch: "feature/a"}, 10); err != nil {
		t.Fatal(err)
	} else if truncated {
		t.Fatal("empty branch response should not be truncated")
	}
	if strings.Contains(endpoint, "/projects/") || !strings.Contains(endpoint, "/merge_requests?") || !strings.Contains(endpoint, "scope=all") || !strings.Contains(endpoint, "source_branch=feature%2Fa") || !strings.Contains(endpoint, "page=1") {
		t.Fatalf("upstream endpoint = %q, want global merge request list", endpoint)
	}
	if _, truncated, err := c.listBranchMergeRequests(context.Background(), branchSearch{projectID: "10", targetBranch: "feature/a"}, 10); err != nil {
		t.Fatal(err)
	} else if truncated {
		t.Fatal("empty branch response should not be truncated")
	}
	if !strings.Contains(endpoint, "/projects/10/merge_requests?") || !strings.Contains(endpoint, "target_branch=feature%2Fa") || !strings.Contains(endpoint, "page=1") {
		t.Fatalf("downstream endpoint = %q", endpoint)
	}
}

func TestRequestBudgetIsSharedAcrossConcurrentAPIRequests(t *testing.T) {
	c := New("")
	budget := newRequestBudget(1)
	ctx := context.WithValue(context.Background(), requestBudgetContextKey{}, budget)
	var mu sync.Mutex
	calls := 0
	c.Runner = RunnerFunc(func(_ context.Context, _ []string) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return []byte(`{}`), nil
	})

	const workers = 12
	var wg sync.WaitGroup
	var succeeded, rejected int
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var response rawProject
			err := c.apiJSON(ctx, "concurrent fixture", "/projects/10", &response)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				succeeded++
			} else if errors.Is(err, ErrRequestBudgetExceeded) {
				rejected++
			} else {
				t.Errorf("unexpected error = %v", err)
			}
		}()
	}
	wg.Wait()
	if succeeded != 1 || rejected != workers-1 || calls != 1 {
		t.Fatalf("successes/rejections/calls = %d/%d/%d, want 1/%d/1", succeeded, rejected, calls, workers-1)
	}
}

func TestLoadRequestBudgetResetsForEachRefresh(t *testing.T) {
	c := New("")
	c.MaxRequests = 1
	var mu sync.Mutex
	calls := 0
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		if args[len(args)-1] == "/user" {
			return []byte(`{"username":"me"}`), nil
		}
		return []byte(`[]`), nil
	})
	for i := 0; i < 2; i++ {
		_, err := c.Load(context.Background(), graph.SearchOptions{Authored: true})
		if !errors.Is(err, ErrRequestBudgetExceeded) {
			t.Fatalf("refresh %d error = %v, want request budget error", i+1, err)
		}
	}
	if calls != 2 {
		t.Fatalf("subprocess calls = %d, want one /user call per refresh", calls)
	}
}

func TestAPIRequestTimeoutIsAppliedToRunner(t *testing.T) {
	c := New("")
	c.RequestTimeout = 10 * time.Millisecond
	c.Runner = RunnerFunc(func(ctx context.Context, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	var response rawUser
	err := c.apiJSON(context.Background(), "timeout fixture", "/user", &response)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
}

func TestRefreshTimeoutBoundsLoadContext(t *testing.T) {
	c := New("")
	c.RefreshTimeout = 10 * time.Millisecond
	c.RequestTimeout = time.Second
	c.Runner = RunnerFunc(func(ctx context.Context, _ []string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_, err := c.Load(context.Background(), graph.SearchOptions{Authored: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want refresh deadline exceeded", err)
	}
}

func TestStackDiscoveryReportsBudgetAndStops(t *testing.T) {
	c := New("")
	c.MaxMRs = 10
	c.Runner = RunnerFunc(func(_ context.Context, _ []string) ([]byte, error) {
		t.Fatal("request runner should not be called after the budget is exhausted")
		return nil, nil
	})
	seed := &graph.PullRequest{
		ID: "pr:seed", RepositoryID: "10", HeadRepositoryID: "10", HeadRefName: "feature",
		BaseRefName: "main", DefaultBranch: "main",
	}
	byID := map[string]*graph.PullRequest{seed.ID: seed}
	ctx := context.WithValue(context.Background(), requestBudgetContextKey{}, newRequestBudget(0))
	_, warnings := c.discoverStacks(ctx, []*graph.PullRequest{seed}, byID, "viewer", nil)
	if len(warnings) != 1 || warnings[0] != "GitLab request budget exhausted; stack discovery stopped." {
		t.Fatalf("warnings = %v, want budget stop warning", warnings)
	}
}

func TestExactProjectAndBranchIdentity(t *testing.T) {
	child := &graph.PullRequest{RepositoryID: "10", BaseRefName: "stack-1"}
	if !isUpstreamParent(&graph.PullRequest{RepositoryID: "11", HeadRepositoryID: "10", HeadRefName: "stack-1"}, child) {
		t.Fatal("cross-project parent was not accepted")
	}
	if isUpstreamParent(&graph.PullRequest{RepositoryID: "99", HeadRepositoryID: "99", HeadRefName: "stack-1"}, child) {
		t.Fatal("parent from a different source project was accepted")
	}
	if isUpstreamParent(&graph.PullRequest{RepositoryID: "10", HeadRepositoryID: "", HeadRefName: "stack-1"}, child) {
		t.Fatal("parent without a source project was accepted")
	}
	parent := &graph.PullRequest{HeadRepositoryID: "10", HeadRefName: "stack-1"}
	if !isDownstreamChild(parent, &graph.PullRequest{RepositoryID: "10", BaseRefName: "stack-1", HeadRepositoryID: "11"}) {
		t.Fatal("same-project downstream child was not accepted")
	}
	if isDownstreamChild(parent, &graph.PullRequest{RepositoryID: "99", BaseRefName: "stack-1"}) {
		t.Fatal("fork downstream child was accepted")
	}
}

func TestLoadHonorsMaxMRsAndReportsWarning(t *testing.T) {
	c := New("")
	c.MaxMRs = 2
	c.MaxDepth = 1
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case endpoint == "/user":
			return []byte(`{"username":"viewer"}`), nil
		case strings.Contains(endpoint, "scope=created_by_me"):
			return []byte(`[
{"id":801,"iid":1,"state":"opened","title":"First","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"feature-one","web_url":"https://gitlab.example/acme/app/-/merge_requests/1"},
{"id":802,"iid":2,"state":"opened","title":"Second","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"feature-two","web_url":"https://gitlab.example/acme/app/-/merge_requests/2"},
{"id":803,"iid":3,"state":"opened","title":"Third","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"feature-three","web_url":"https://gitlab.example/acme/app/-/merge_requests/3"}
]`), nil
		case strings.HasSuffix(endpoint, "/approvals"):
			return []byte(`{"approvals_required":0,"approvals_left":0,"approved_by":[]}`), nil
		case strings.Contains(endpoint, "/merge_requests/"):
			return []byte(`{"state":"opened"}`), nil
		case endpoint == "/projects/10":
			return []byte(`{"id":10,"path_with_namespace":"acme/app","web_url":"https://gitlab.example/acme/app","default_branch":"main"}`), nil
		case strings.Contains(endpoint, "target_branch="):
			return []byte(`[]`), nil
		default:
			return []byte(`[]`), nil
		}
	})
	result, err := c.Load(context.Background(), graph.SearchOptions{Authored: true})
	if err != nil {
		t.Fatal(err)
	}
	mergeRequests := 0
	for _, node := range result.Nodes {
		if node.Kind == "pullRequest" {
			mergeRequests++
		}
	}
	if mergeRequests != 2 {
		t.Fatalf("graph merge-request count = %d, want 2: %+v", mergeRequests, result)
	}
	if !strings.Contains(strings.Join(result.Warnings, "\n"), "Merge request limit reached") {
		t.Fatalf("warnings = %v, want MaxMRs warning", result.Warnings)
	}
}

func TestRawStackDiscoveryHonorsMaxDepth(t *testing.T) {
	c := New("")
	c.MaxMRs = 10
	c.MaxDepth = 1
	var branchQueries []string
	var branchMu sync.Mutex
	unexpectedNextDepth := false
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		if strings.Contains(endpoint, "target_branch=") {
			branchMu.Lock()
			branchQueries = append(branchQueries, endpoint)
			branchMu.Unlock()
			if strings.Contains(endpoint, "target_branch=feature-seed") {
				return []byte(`[{"id":902,"iid":2,"state":"opened","title":"Child","target_project_id":20,"source_project_id":20,"target_branch":"feature-seed","source_branch":"feature-next","target_project":{"id":20,"path_with_namespace":"acme/app-fork","default_branch":"main"},"source_project":{"id":20,"path_with_namespace":"acme/app-fork","default_branch":"main"}}]`), nil
			}
			if strings.Contains(endpoint, "target_branch=feature-next") {
				branchMu.Lock()
				unexpectedNextDepth = true
				branchMu.Unlock()
			}
		}
		return []byte(`[]`), nil
	})
	seed := rawMergeRequest{
		ID: 901, IID: 1, State: "opened", TargetProjectID: 10, SourceProjectID: 20,
		TargetBranch: "main", SourceBranch: "feature-seed",
		TargetProject: &rawProject{ID: 10, PathWithNamespace: "acme/app", DefaultBranch: "main"},
		SourceProject: &rawProject{ID: 20, PathWithNamespace: "acme/app-fork", DefaultBranch: "main"},
	}
	seedID := rawMRID(seed)
	rawByID := map[string]rawMergeRequest{seedID: seed}
	sourceByID := map[string]string{seedID: "search"}
	limitReached, warnings := c.discoverRawStacks(context.Background(), []string{seedID}, rawByID, sourceByID, "viewer", nil)
	if limitReached || len(warnings) != 0 {
		t.Fatalf("limit/warnings = %v/%v, want false/none", limitReached, warnings)
	}
	if len(rawByID) != 2 {
		t.Fatalf("discovered merge requests = %d, want seed and one child", len(rawByID))
	}
	branchMu.Lock()
	queryCount := len(branchQueries)
	unexpected := unexpectedNextDepth
	branchMu.Unlock()
	if unexpected {
		t.Fatal("MaxDepth allowed a next-depth branch request")
	}
	if queryCount != 1 {
		t.Fatalf("branch query count = %d, want only first depth", queryCount)
	}
}

func TestRawStackDiscoveryDoesNotGuessUnknownDefaultBranch(t *testing.T) {
	c := New("")
	c.MaxMRs = 10
	c.MaxDepth = 1
	var upstreamQueries int
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		if strings.Contains(args[len(args)-1], "source_branch=unknown-base") {
			upstreamQueries++
		}
		return []byte(`[]`), nil
	})
	seed := rawMergeRequest{
		ID: 903, IID: 3, State: "opened", TargetProjectID: 10, SourceProjectID: 10,
		TargetBranch: "unknown-base", SourceBranch: "",
	}
	seedID := rawMRID(seed)
	rawByID := map[string]rawMergeRequest{seedID: seed}
	sourceByID := map[string]string{seedID: "search"}
	if limitReached, warnings := c.discoverRawStacks(context.Background(), []string{seedID}, rawByID, sourceByID, "viewer", nil); limitReached || len(warnings) != 0 {
		t.Fatalf("limit/warnings = %v/%v, want false/none", limitReached, warnings)
	}
	if upstreamQueries != 0 {
		t.Fatalf("upstream queries with unknown default branch = %d, want 0", upstreamQueries)
	}
}

func TestStackDiscoveryHonorsMaxDepth(t *testing.T) {
	c := New("")
	c.MaxMRs = 10
	c.MaxDepth = 1
	var branchQueries []string
	var branchMu sync.Mutex
	unexpectedNextDepth := false
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case strings.Contains(endpoint, "target_branch=feature-seed"):
			branchMu.Lock()
			branchQueries = append(branchQueries, endpoint)
			branchMu.Unlock()
			return []byte(`[{"id":1002,"iid":2,"state":"opened","title":"Child","target_project_id":20,"source_project_id":20,"target_branch":"feature-seed","source_branch":"feature-next"}]`), nil
		case strings.Contains(endpoint, "target_branch=feature-next"):
			branchMu.Lock()
			unexpectedNextDepth = true
			branchMu.Unlock()
			return []byte(`[]`), nil
		case strings.HasSuffix(endpoint, "/approvals"):
			return []byte(`{"approvals_required":0,"approvals_left":0,"approved_by":[]}`), nil
		case strings.Contains(endpoint, "/merge_requests/"):
			return []byte(`{"id":1002,"iid":2,"state":"opened","title":"Child","target_project_id":20,"source_project_id":20,"target_branch":"feature-seed","source_branch":"feature-next"}`), nil
		case endpoint == "/projects/20":
			return []byte(`{"id":20,"path_with_namespace":"acme/app-fork","web_url":"https://gitlab.example/acme/app-fork","default_branch":"main"}`), nil
		default:
			return []byte(`[]`), nil
		}
	})
	seed := &graph.PullRequest{
		ID: "seed", RepositoryID: "10", HeadRepositoryID: "20", HeadRefName: "feature-seed",
		BaseRefName: "main", DefaultBranch: "main",
	}
	byID := map[string]*graph.PullRequest{seed.ID: seed}
	limitReached, warnings := c.discoverStacks(context.Background(), []*graph.PullRequest{seed}, byID, "viewer", nil)
	if limitReached || len(warnings) != 0 {
		t.Fatalf("limit/warnings = %v/%v, want false/none", limitReached, warnings)
	}
	branchMu.Lock()
	queryCount := len(branchQueries)
	unexpected := unexpectedNextDepth
	branchMu.Unlock()
	if unexpected {
		t.Fatal("MaxDepth allowed a next-depth branch request")
	}
	if len(byID) != 2 || queryCount != 1 {
		t.Fatalf("graph/query counts = %d/%d, want 2/1", len(byID), queryCount)
	}
}

func TestApprovalFailureKeepsMergeRequest(t *testing.T) {
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		if strings.HasSuffix(endpoint, "/approvals") {
			return nil, errors.New("403 forbidden")
		}
		if strings.Contains(endpoint, "/merge_requests/") {
			return []byte(`{"id":3,"iid":4,"target_project_id":10,"source_project_id":10,"source_branch":"topic","target_branch":"main"}`), nil
		}
		return []byte(`[]`), nil
	})
	mr := rawMergeRequest{ID: 3, IID: 4, TargetProjectID: 10, SourceProjectID: 10, SourceBranch: "topic", TargetBranch: "main"}
	got, approval, detailErr, approvalErr := c.hydrateOne(context.Background(), mr)
	if detailErr != nil || approval != nil || approvalErr == nil || got.ID != 3 {
		t.Fatalf("hydrate = %+v approval=%v detailErr=%v approvalErr=%v", got, approval, detailErr, approvalErr)
	}
	converted := c.convert(got, approval, "viewer", "search")
	if converted == nil || converted.ApprovalState != "UNAVAILABLE" {
		t.Fatalf("approval state = %v, want UNAVAILABLE", converted)
	}
}

func TestProjectLookupIsCached(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		if args[len(args)-1] != "/projects/10" {
			return []byte(`{}`), nil
		}
		mu.Lock()
		calls++
		mu.Unlock()
		return []byte(`{"id":10,"path_with_namespace":"acme/db","default_branch":"main"}`), nil
	})
	if _, err := c.project(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if _, err := c.project(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("project API calls = %d, want 1", calls)
	}
}

func TestLoadDedupeAndBuildsExactStack(t *testing.T) {
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case endpoint == "/user":
			return []byte(`{"username":"me"}`), nil
		case strings.Contains(endpoint, "scope=created_by_me"):
			return []byte(`[ {"id":101,"iid":1,"state":"opened","title":"Base","web_url":"https://gitlab.test/acme/app/-/merge_requests/1","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"feature-a","author":{"username":"me"}} ]`), nil
		case strings.Contains(endpoint, "scope=assigned_to_me"):
			return []byte(`[ {"id":101,"iid":1,"state":"opened","title":"Base","web_url":"https://gitlab.test/acme/app/-/merge_requests/1","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"feature-a","author":{"username":"me"}}, {"id":102,"iid":2,"state":"opened","title":"Child","web_url":"https://gitlab.test/acme/app/-/merge_requests/2","target_project_id":10,"source_project_id":10,"target_branch":"feature-a","source_branch":"feature-b","author":{"username":"other"}} ]`), nil
		case strings.Contains(endpoint, "scope=reviews_for_me"):
			return []byte(`[ {"id":102,"iid":2,"state":"opened","title":"Child","web_url":"https://gitlab.test/acme/app/-/merge_requests/2","target_project_id":10,"source_project_id":10,"target_branch":"feature-a","source_branch":"feature-b","author":{"username":"other"},"reviewers":[{"username":"me"}]} ]`), nil
		case strings.HasSuffix(endpoint, "/approvals"):
			return []byte(`{"approvals_required":1,"approvals_left":1,"approved_by":[]}`), nil
		case strings.Contains(endpoint, "/merge_requests/"):
			if strings.HasSuffix(endpoint, "/1") {
				return []byte(`{"id":101,"iid":1,"state":"opened","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"feature-a","title":"Base","web_url":"https://gitlab.test/acme/app/-/merge_requests/1"}`), nil
			}
			return []byte(`{"id":102,"iid":2,"state":"opened","target_project_id":10,"source_project_id":10,"target_branch":"feature-a","source_branch":"feature-b","title":"Child","web_url":"https://gitlab.test/acme/app/-/merge_requests/2","reviewers":[{"username":"me"}]}`), nil
		case endpoint == "/projects/10":
			return []byte(`{"id":10,"path_with_namespace":"acme/app","web_url":"https://gitlab.test/acme/app","default_branch":"main"}`), nil
		case strings.HasPrefix(endpoint, "/projects/10/merge_requests?"):
			return []byte(`[]`), nil
		default:
			return nil, errors.New("unexpected fixture endpoint: " + endpoint)
		}
	})
	result, err := c.Load(context.Background(), graph.SearchOptions{Authored: true, Assigned: true, ReviewRequested: true})
	if err != nil {
		t.Fatal(err)
	}
	prs, edges := 0, 0
	for _, node := range result.Nodes {
		if node.Kind == "pullRequest" {
			prs++
		}
	}
	for _, edge := range result.Edges {
		if edge.Source == "pr:gitlab:mr:101" && edge.Target == "pr:gitlab:mr:102" {
			edges++
		}
	}
	if prs != 2 || edges != 1 {
		t.Fatalf("graph contains %d MRs and %d stack edges, want 2 and 1: %+v", prs, edges, result)
	}
}

func TestLoadDiscoversCrossProjectUpstreamParentFromGlobalList(t *testing.T) {
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case endpoint == "/user":
			return []byte(`{"username":"me"}`), nil
		case strings.Contains(endpoint, "scope=assigned_to_me"):
			return []byte(`[ {"id":401,"iid":1,"state":"opened","title":"Fork child","web_url":"https://gitlab.test/acme/fork/-/merge_requests/1","target_project_id":11,"source_project_id":11,"target_branch":"feature-parent","source_branch":"feature-child","author":{"username":"other"}} ]`), nil
		case strings.HasSuffix(endpoint, "/approvals"):
			return []byte(`{"approvals_required":0,"approvals_left":0,"approved_by":[]}`), nil
		case strings.Contains(endpoint, "/merge_requests/"):
			if strings.HasSuffix(endpoint, "/1") {
				return []byte(`{"id":401,"iid":1,"state":"opened","target_project_id":11,"source_project_id":11,"target_branch":"feature-parent","source_branch":"feature-child","title":"Fork child","web_url":"https://gitlab.test/acme/fork/-/merge_requests/1"}`), nil
			}
			return []byte(`{"id":402,"iid":2,"state":"opened","target_project_id":10,"source_project_id":11,"target_branch":"main","source_branch":"feature-parent","title":"Upstream parent","web_url":"https://gitlab.test/acme/upstream/-/merge_requests/2"}`), nil
		case endpoint == "/projects/10":
			return []byte(`{"id":10,"path_with_namespace":"acme/upstream","web_url":"https://gitlab.test/acme/upstream","default_branch":"main"}`), nil
		case endpoint == "/projects/11":
			return []byte(`{"id":11,"path_with_namespace":"acme/fork","web_url":"https://gitlab.test/acme/fork","default_branch":"main"}`), nil
		case strings.Contains(endpoint, "source_branch=feature-parent"):
			if strings.Contains(endpoint, "/projects/") {
				t.Fatalf("upstream discovery used a project-scoped endpoint: %s", endpoint)
			}
			return []byte(`[ {"id":402,"iid":2,"state":"opened","target_project_id":10,"source_project_id":11,"target_branch":"main","source_branch":"feature-parent","title":"Upstream parent","web_url":"https://gitlab.test/acme/upstream/-/merge_requests/2"}, {"id":403,"iid":3,"state":"opened","target_project_id":99,"source_project_id":99,"target_branch":"main","source_branch":"feature-parent","title":"Wrong fork","web_url":"https://gitlab.test/other/fork/-/merge_requests/3"} ]`), nil
		case strings.HasPrefix(endpoint, "/projects/11/merge_requests?"):
			return []byte(`[]`), nil
		default:
			return nil, errors.New("unexpected fixture endpoint: " + endpoint)
		}
	})
	result, err := c.Load(context.Background(), graph.SearchOptions{Assigned: true})
	if err != nil {
		t.Fatal(err)
	}
	var parent *graph.PullRequest
	for _, node := range result.Nodes {
		if node.Kind == "pullRequest" && node.PR.Number == 2 {
			parent = node.PR
			break
		}
	}
	if parent == nil {
		t.Fatalf("cross-project upstream parent was not discovered: %+v", result)
	}
	if parent.RepositoryID != "10" || parent.HeadRepositoryID != "11" || parent.HeadRefName != "feature-parent" {
		t.Fatalf("parent identity = target=%q source=%q branch=%q", parent.RepositoryID, parent.HeadRepositoryID, parent.HeadRefName)
	}
	foundStackEdge := false
	for _, edge := range result.Edges {
		if edge.Source == "pr:"+parent.ID && edge.Target == "pr:gitlab:mr:401" {
			foundStackEdge = true
			break
		}
	}
	if !foundStackEdge {
		t.Fatalf("edges = %+v, want parent -> child", result.Edges)
	}
}

func TestUpstreamDiscoveryDoesNotFanOutIntoParentSiblings(t *testing.T) {
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case endpoint == "/user":
			return []byte(`{"username":"me"}`), nil
		case strings.Contains(endpoint, "scope=assigned_to_me"):
			return []byte(`[ {"id":301,"iid":1,"state":"opened","title":"Seed child","web_url":"https://gitlab.test/acme/app/-/merge_requests/1","target_project_id":10,"source_project_id":10,"target_branch":"feature-parent","source_branch":"feature-seed","author":{"username":"other"}} ]`), nil
		case strings.HasSuffix(endpoint, "/approvals"):
			return []byte(`{"approvals_required":0,"approvals_left":0,"approved_by":[]}`), nil
		case strings.Contains(endpoint, "/merge_requests/"):
			switch {
			case strings.HasSuffix(endpoint, "/1"):
				return []byte(`{"id":301,"iid":1,"state":"opened","target_project_id":10,"source_project_id":10,"target_branch":"feature-parent","source_branch":"feature-seed","title":"Seed child","web_url":"https://gitlab.test/acme/app/-/merge_requests/1"}`), nil
			case strings.HasSuffix(endpoint, "/2"):
				return []byte(`{"id":302,"iid":2,"state":"opened","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"feature-parent","title":"Parent","web_url":"https://gitlab.test/acme/app/-/merge_requests/2"}`), nil
			default:
				return []byte(`{"id":303,"iid":3,"state":"opened","target_project_id":10,"source_project_id":10,"target_branch":"feature-parent","source_branch":"sibling","title":"Sibling","web_url":"https://gitlab.test/acme/app/-/merge_requests/3"}`), nil
			}
		case endpoint == "/projects/10":
			return []byte(`{"id":10,"path_with_namespace":"acme/app","web_url":"https://gitlab.test/acme/app","default_branch":"main"}`), nil
		case strings.Contains(endpoint, "source_branch=feature-parent"):
			if strings.Contains(endpoint, "/projects/") {
				t.Fatalf("upstream discovery used a project-scoped endpoint: %s", endpoint)
			}
			return []byte(`[ {"id":302,"iid":2,"state":"opened","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"feature-parent","title":"Parent","web_url":"https://gitlab.test/acme/app/-/merge_requests/2"} ]`), nil
		case strings.Contains(endpoint, "target_branch=feature-parent"):
			return []byte(`[ {"id":303,"iid":3,"state":"opened","target_project_id":10,"source_project_id":10,"target_branch":"feature-parent","source_branch":"sibling","title":"Sibling","web_url":"https://gitlab.test/acme/app/-/merge_requests/3"} ]`), nil
		case strings.HasPrefix(endpoint, "/projects/10/merge_requests?"):
			return []byte(`[]`), nil
		default:
			return nil, errors.New("unexpected fixture endpoint: " + endpoint)
		}
	})
	result, err := c.Load(context.Background(), graph.SearchOptions{Assigned: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range result.Nodes {
		if node.Kind == "pullRequest" && node.PR.Number == 3 {
			t.Fatal("parent sibling was discovered from an upstream-only node")
		}
	}
	count := 0
	for _, node := range result.Nodes {
		if node.Kind == "pullRequest" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("merge request count = %d, want seed and parent only", count)
	}
}

func TestSearchSpecsRunConcurrently(t *testing.T) {
	c := New("")
	var mu sync.Mutex
	started := 0
	progress := make([]string, 0, 5)
	release := make(chan struct{})
	var releaseOnce sync.Once
	entered := make(chan struct{}, 4)
	c.Runner = RunnerFunc(func(ctx context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		if endpoint == "/user" {
			return []byte(`{"username":"me"}`), nil
		}
		if !strings.Contains(endpoint, "scope=") {
			return []byte(`[]`), nil
		}
		mu.Lock()
		started++
		count := started
		mu.Unlock()
		entered <- struct{}{}
		if count == 4 {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
			return []byte(`[]`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	done := make(chan error, 1)
	go func() {
		_, err := c.LoadProgress(context.Background(), graph.SearchOptions{Authored: true, Assigned: true, ReviewRequested: true, Query: "stack"}, func(current, total int, phase string, collected int) {
			if phase == "Searching merge requests" {
				progress = append(progress, fmt.Sprintf("%d/%d:%d", current, total, collected))
			}
		})
		done <- err
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			releaseOnce.Do(func() { close(release) })
			t.Fatal("search specs did not run concurrently")
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent search load did not finish")
	}
	wantProgress := []string{"0/4:0", "1/4:0", "2/4:0", "3/4:0", "4/4:0"}
	if fmt.Sprint(progress) != fmt.Sprint(wantProgress) {
		t.Fatalf("search progress = %v, want deterministic %v", progress, wantProgress)
	}
}

func TestStackFrontierDeduplicatesBranchJobs(t *testing.T) {
	c := New("")
	c.MaxDepth = 1
	var mu sync.Mutex
	calls := 0
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		if strings.Contains(args[len(args)-1], "target_branch=shared-feature") {
			mu.Lock()
			calls++
			mu.Unlock()
		}
		return []byte(`[]`), nil
	})
	seeds := []*graph.PullRequest{
		{ID: "seed-a", RepositoryID: "10", HeadRepositoryID: "20", HeadRefName: "shared-feature", BaseRefName: "main", DefaultBranch: "main"},
		{ID: "seed-b", RepositoryID: "11", HeadRepositoryID: "20", HeadRefName: "shared-feature", BaseRefName: "main", DefaultBranch: "main"},
	}
	byID := map[string]*graph.PullRequest{seeds[0].ID: seeds[0], seeds[1].ID: seeds[1]}
	c.discoverStacks(context.Background(), seeds, byID, "viewer", nil)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("duplicate branch API calls = %d, want 1", calls)
	}
}

func TestStackBranchJobsRunConcurrentlyWithBoundedWorkers(t *testing.T) {
	c := New("")
	c.MaxMRs = 100
	c.MaxDepth = 1
	const jobCount = maxConcurrentGitLabRequests + 2
	var mu sync.Mutex
	active, maxActive, started := 0, 0, 0
	release := make(chan struct{})
	var releaseOnce sync.Once
	entered := make(chan struct{}, jobCount)
	c.Runner = RunnerFunc(func(ctx context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		if !strings.Contains(endpoint, "target_branch=feature-") {
			return []byte(`[]`), nil
		}
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		started++
		count := started
		mu.Unlock()
		entered <- struct{}{}
		if count == maxConcurrentGitLabRequests {
			releaseOnce.Do(func() { close(release) })
		}
		select {
		case <-release:
			mu.Lock()
			active--
			mu.Unlock()
			return []byte(`[]`), nil
		case <-ctx.Done():
			mu.Lock()
			active--
			mu.Unlock()
			return nil, ctx.Err()
		}
	})

	seeds := make([]*graph.PullRequest, 0, jobCount)
	byID := make(map[string]*graph.PullRequest, jobCount)
	for i := 0; i < jobCount; i++ {
		seed := &graph.PullRequest{
			ID:               fmt.Sprintf("seed-%d", i),
			RepositoryID:     "10",
			HeadRepositoryID: fmt.Sprintf("%d", 20+i),
			HeadRefName:      fmt.Sprintf("feature-%d", i),
			BaseRefName:      "main",
			DefaultBranch:    "main",
		}
		seeds = append(seeds, seed)
		byID[seed.ID] = seed
	}
	done := make(chan struct{})
	go func() {
		c.discoverStacks(context.Background(), seeds, byID, "viewer", nil)
		close(done)
	}()
	for i := 0; i < jobCount; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			releaseOnce.Do(func() { close(release) })
			t.Fatal("stack branch jobs did not run concurrently")
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stack discovery did not finish")
	}
	mu.Lock()
	gotMaxActive := maxActive
	mu.Unlock()
	if gotMaxActive != maxConcurrentGitLabRequests {
		t.Fatalf("max branch-list concurrency = %d, want %d", gotMaxActive, maxConcurrentGitLabRequests)
	}
}

func TestStackCandidatesHydrateAsOneBatch(t *testing.T) {
	c := New("")
	c.MaxMRs = 10
	c.MaxDepth = 1
	var detailStarted int
	var mu sync.Mutex
	releaseDetails := make(chan struct{})
	var releaseOnce sync.Once
	entered := make(chan struct{}, 2)
	c.Runner = RunnerFunc(func(ctx context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case strings.Contains(endpoint, "target_branch=feature"):
			return []byte(`[
{"id":501,"iid":1,"state":"opened","target_project_id":20,"source_project_id":20,"target_branch":"feature","source_branch":"child-a","title":"Child A"},
{"id":502,"iid":2,"state":"opened","target_project_id":20,"source_project_id":20,"target_branch":"feature","source_branch":"child-b","title":"Child B"}
]`), nil
		case strings.Contains(endpoint, "/merge_requests/") && !strings.HasSuffix(endpoint, "/approvals"):
			mu.Lock()
			detailStarted++
			count := detailStarted
			mu.Unlock()
			entered <- struct{}{}
			if count == 2 {
				releaseOnce.Do(func() { close(releaseDetails) })
			}
			select {
			case <-releaseDetails:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if strings.HasSuffix(endpoint, "/1") {
				return []byte(`{"id":501,"iid":1,"state":"opened","target_project_id":20,"source_project_id":20,"target_branch":"feature","source_branch":"child-a","title":"Child A"}`), nil
			}
			return []byte(`{"id":502,"iid":2,"state":"opened","target_project_id":20,"source_project_id":20,"target_branch":"feature","source_branch":"child-b","title":"Child B"}`), nil
		case strings.HasSuffix(endpoint, "/approvals"):
			return []byte(`{"approvals_required":0,"approvals_left":0,"approved_by":[]}`), nil
		case strings.HasPrefix(endpoint, "/projects/20"):
			return []byte(`{"id":20,"path_with_namespace":"acme/app","default_branch":"main"}`), nil
		default:
			return []byte(`[]`), nil
		}
	})

	seed := &graph.PullRequest{ID: "seed", RepositoryID: "10", HeadRepositoryID: "20", HeadRefName: "feature", BaseRefName: "main", DefaultBranch: "main"}
	byID := map[string]*graph.PullRequest{seed.ID: seed}
	done := make(chan struct{})
	go func() {
		c.discoverStacks(context.Background(), []*graph.PullRequest{seed}, byID, "viewer", nil)
		close(done)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			releaseOnce.Do(func() { close(releaseDetails) })
			t.Fatal("discovered candidates were hydrated one at a time")
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stack candidate hydration did not finish")
	}
	if len(byID) != 3 {
		t.Fatalf("discovered merge requests = %d, want seed plus two children", len(byID))
	}
}

func TestProjectLookupSingleflightAndRetryAfterFailure(t *testing.T) {
	c := New("")
	var mu sync.Mutex
	calls := 0
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	c.Runner = RunnerFunc(func(ctx context.Context, args []string) ([]byte, error) {
		if args[len(args)-1] != "/projects/10" {
			return []byte(`{}`), nil
		}
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			startOnce.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if call == 1 {
			return nil, errors.New("temporary project failure")
		}
		return []byte(`{"id":10,"path_with_namespace":"acme/app","default_branch":"main"}`), nil
	})

	const waiters = 8
	results := make(chan error, waiters)
	requestCtx := context.WithValue(context.Background(), requestBudgetContextKey{}, newRequestBudget(1))
	for i := 0; i < waiters; i++ {
		go func() {
			_, err := c.project(requestCtx, 10)
			results <- err
		}()
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("project request did not start")
	}
	close(release)
	for i := 0; i < waiters; i++ {
		if err := <-results; err == nil {
			t.Fatal("first failed project lookup unexpectedly succeeded")
		}
	}
	mu.Lock()
	firstCalls := calls
	mu.Unlock()
	if firstCalls != 1 {
		t.Fatalf("concurrent project API calls = %d, want 1", firstCalls)
	}
	if _, err := c.project(context.Background(), 10); err != nil {
		t.Fatalf("retry after project failure = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("project API calls after retry = %d, want 2", calls)
	}
}

func TestProjectLookupWaiterHonorsContextCancellation(t *testing.T) {
	c := New("")
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	c.Runner = RunnerFunc(func(ctx context.Context, args []string) ([]byte, error) {
		if args[len(args)-1] != "/projects/10" {
			return []byte(`{}`), nil
		}
		once.Do(func() { close(started) })
		select {
		case <-release:
			return []byte(`{"id":10}`), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	creatorDone := make(chan error, 1)
	go func() {
		_, err := c.project(context.Background(), 10)
		creatorDone <- err
	}()
	<-started
	waiterCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.project(waiterCtx, 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter error = %v, want context deadline exceeded", err)
	}
	close(release)
	if err := <-creatorDone; err != nil {
		t.Fatalf("creator error = %v", err)
	}
}

func TestDiscoveredHydrationWarningsAreRetained(t *testing.T) {
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case strings.Contains(endpoint, "target_branch=feature"):
			return []byte(`[{"id":601,"iid":1,"state":"opened","target_project_id":20,"source_project_id":20,"target_branch":"feature","source_branch":"child"}]`), nil
		case strings.HasSuffix(endpoint, "/approvals"):
			return nil, errors.New("approval unavailable")
		case strings.Contains(endpoint, "/merge_requests/"):
			return nil, errors.New("detail unavailable")
		default:
			return []byte(`{"id":20,"path_with_namespace":"acme/app","default_branch":"main"}`), nil
		}
	})
	seed := &graph.PullRequest{ID: "seed", RepositoryID: "10", HeadRepositoryID: "20", HeadRefName: "feature", BaseRefName: "main", DefaultBranch: "main"}
	byID := map[string]*graph.PullRequest{seed.ID: seed}
	_, warnings := c.discoverStacks(context.Background(), []*graph.PullRequest{seed}, byID, "viewer", nil)
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "details could not be loaded") || !strings.Contains(joined, "approval data is unavailable") {
		t.Fatalf("discovered hydration warnings = %v", warnings)
	}
}

func TestLoadStagesEmitsCrossProjectTopologyBeforeHydration(t *testing.T) {
	c := New("")
	c.MaxDepth = 3
	topologyEmitted := false
	var statusBeforeTopology atomic.Int32
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case endpoint == "/user":
			return []byte(`{"username":"viewer"}`), nil
		case strings.Contains(endpoint, "scope=created_by_me"):
			return []byte(`[{"id":702,"iid":2,"state":"opened","title":"Child","target_project_id":11,"source_project_id":11,"target_branch":"stack-base","source_branch":"child-feature","reviewers":[{"username":"viewer"}]}]`), nil
		case strings.Contains(endpoint, "source_branch=stack-base"):
			return []byte(`[{"id":701,"iid":1,"state":"opened","title":"Parent","target_project_id":10,"source_project_id":11,"target_branch":"main","source_branch":"stack-base"}]`), nil
		case strings.Contains(endpoint, "target_branch=child-feature"):
			return []byte(`[]`), nil
		case endpoint == "/projects/10":
			return []byte(`{"id":10,"path_with_namespace":"upstream/app","web_url":"https://gitlab.example/upstream/app","default_branch":"main"}`), nil
		case endpoint == "/projects/11":
			return []byte(`{"id":11,"path_with_namespace":"fork/app","web_url":"https://gitlab.example/fork/app","default_branch":"main"}`), nil
		case strings.Contains(endpoint, "/merge_requests/"):
			if !topologyEmitted {
				statusBeforeTopology.Add(1)
			}
			if strings.HasSuffix(endpoint, "/approvals") {
				return []byte(`{"approvals_required":1,"approvals_left":0,"approved":true,"approved_by":[{"user":{"username":"alice"}}]}`), nil
			}
			if strings.Contains(endpoint, "/projects/10/") {
				return []byte(`{"id":701,"iid":1,"state":"opened","title":"Parent","target_project_id":10,"source_project_id":11,"target_branch":"main","source_branch":"stack-base"}`), nil
			}
			return []byte(`{"id":702,"iid":2,"state":"opened","title":"Child","target_project_id":11,"source_project_id":11,"target_branch":"stack-base","source_branch":"child-feature","reviewers":[{"username":"viewer"}]}`), nil
		default:
			return []byte(`[]`), nil
		}
	})

	var topology graph.Result
	final, err := c.LoadStages(context.Background(), graph.SearchOptions{Authored: true}, nil, func(stage string, result graph.Result) {
		if stage == "topology" {
			topology = result
			topologyEmitted = true
			for _, node := range result.Nodes {
				if node.PR != nil && node.PR.ApprovalState != "LOADING" {
					t.Errorf("topology approval state = %q, want LOADING", node.PR.ApprovalState)
				}
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !topologyEmitted {
		t.Fatal("topology stage was not emitted")
	}
	if statusBeforeTopology.Load() != 0 {
		t.Fatalf("status endpoints called before topology emit = %d", statusBeforeTopology.Load())
	}
	topologyIDs, topologyEdges := graphIdentity(topology)
	finalIDs, finalEdges := graphIdentity(final)
	if fmt.Sprint(topologyIDs) != fmt.Sprint(finalIDs) || fmt.Sprint(topologyEdges) != fmt.Sprint(finalEdges) {
		t.Fatalf("topology identity %v/%v differs from final %v/%v", topologyIDs, topologyEdges, finalIDs, finalEdges)
	}
	prs := resultPullRequests(final)
	if len(prs) != 2 || prs["gitlab:mr:701"].Source != "upstream" || prs["gitlab:mr:702"].Relation != "review-requested" {
		t.Fatalf("final pull requests = %+v", prs)
	}
	for _, pr := range prs {
		if pr.ApprovalState != "APPROVED" {
			t.Errorf("final approval state for %s = %q, want APPROVED", pr.ID, pr.ApprovalState)
		}
	}
}

func TestLoadStagesKeepsTopologyWhenStatusBudgetIsExhausted(t *testing.T) {
	c := New("")
	c.MaxRequests = 3
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint := args[len(args)-1]
		switch {
		case endpoint == "/user":
			return []byte(`{"username":"viewer"}`), nil
		case strings.Contains(endpoint, "scope=created_by_me"):
			return []byte(`[{"id":801,"iid":1,"state":"opened","title":"Feature","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":"","target_project":{"id":10,"path_with_namespace":"acme/app","default_branch":"main"}}]`), nil
		case strings.Contains(endpoint, "/merge_requests/1") && !strings.HasSuffix(endpoint, "/approvals"):
			return []byte(`{"id":801,"iid":1,"state":"opened","title":"Feature","target_project_id":10,"source_project_id":10,"target_branch":"main","source_branch":""}`), nil
		default:
			return []byte(`{}`), nil
		}
	})
	var topology graph.Result
	final, err := c.LoadStages(context.Background(), graph.SearchOptions{Authored: true}, nil, func(stage string, result graph.Result) {
		if stage == "topology" {
			topology = result
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resultPullRequests(topology)) != 1 || resultPullRequests(topology)["gitlab:mr:801"].ApprovalState != "LOADING" {
		t.Fatalf("topology result = %+v", topology)
	}
	pr := resultPullRequests(final)["gitlab:mr:801"]
	if pr == nil || pr.ApprovalState != "UNAVAILABLE" {
		t.Fatalf("final approval = %+v, want unavailable MR retained", pr)
	}
	if !strings.Contains(strings.Join(final.Warnings, "\n"), "approval data is unavailable") {
		t.Fatalf("final warnings = %v", final.Warnings)
	}
}

func resultPullRequests(result graph.Result) map[string]*graph.PullRequest {
	prs := make(map[string]*graph.PullRequest)
	for _, node := range result.Nodes {
		if node.PR != nil {
			prs[node.PR.ID] = node.PR
		}
	}
	return prs
}

func graphIdentity(result graph.Result) ([]string, []string) {
	nodes := make([]string, 0, len(result.Nodes))
	for _, node := range result.Nodes {
		nodes = append(nodes, node.ID)
	}
	edges := make([]string, 0, len(result.Edges))
	for _, edge := range result.Edges {
		edges = append(edges, edge.Source+"->"+edge.Target)
	}
	sort.Strings(nodes)
	sort.Strings(edges)
	return nodes, edges
}
