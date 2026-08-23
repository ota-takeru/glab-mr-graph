package gitlab

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/ota-takeru/glab-mr-graph/internal/graph"
)

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

func TestDecodeJSONPagesAcceptsPaginatedGlabOutput(t *testing.T) {
	var got []rawMergeRequest
	if err := decodeJSONPages([]byte(`[{"id":1,"iid":7}]
[{"id":2,"iid":8}]`), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].IID != 8 {
		t.Fatalf("pages = %+v, want two merge requests", got)
	}
}

func TestAPICommandIncludesPaginateAndHostname(t *testing.T) {
	var got []string
	c := New("gitlab.example.com")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		got = append([]string(nil), args...)
		return []byte(`[]`), nil
	})
	var response []rawMergeRequest
	if err := c.apiJSON(context.Background(), "fixture list", "/merge_requests?scope=all&search=title", true, &response); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(got, " ")
	if !strings.Contains(joined, "api") || !strings.Contains(joined, "--paginate") || !strings.Contains(joined, "--hostname gitlab.example.com") {
		t.Fatalf("glab args = %v", got)
	}
	if got[len(got)-1] != "/merge_requests?scope=all&search=title" {
		t.Fatalf("endpoint = %q", got[len(got)-1])
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

func TestBranchDiscoveryUsesProjectScopedEndpoint(t *testing.T) {
	var endpoint string
	c := New("")
	c.Runner = RunnerFunc(func(_ context.Context, args []string) ([]byte, error) {
		endpoint = args[len(args)-1]
		return []byte(`[]`), nil
	})
	if _, err := c.listBranchMergeRequests(context.Background(), branchSearch{projectID: "10", sourceBranch: "feature/a"}); err != nil {
		t.Fatal(err)
	}
	if endpoint != "/projects/10/merge_requests?per_page=100&source_branch=feature%2Fa&state=opened" {
		t.Fatalf("upstream endpoint = %q", endpoint)
	}
	if _, err := c.listBranchMergeRequests(context.Background(), branchSearch{projectID: "10", targetBranch: "feature/a"}); err != nil {
		t.Fatal(err)
	}
	if endpoint != "/projects/10/merge_requests?per_page=100&state=opened&target_branch=feature%2Fa" {
		t.Fatalf("downstream endpoint = %q", endpoint)
	}
}

func TestExactProjectAndBranchIdentity(t *testing.T) {
	child := &graph.PullRequest{RepositoryID: "10", BaseRefName: "stack-1"}
	if !isUpstreamParent(&graph.PullRequest{RepositoryID: "10", HeadRepositoryID: "10", HeadRefName: "stack-1"}, child) {
		t.Fatal("same-project parent was not accepted")
	}
	if isUpstreamParent(&graph.PullRequest{RepositoryID: "99", HeadRepositoryID: "99", HeadRefName: "stack-1"}, child) {
		t.Fatal("fork parent with same branch was accepted")
	}
	parent := &graph.PullRequest{HeadRepositoryID: "10", HeadRefName: "stack-1"}
	if !isDownstreamChild(parent, &graph.PullRequest{RepositoryID: "10", BaseRefName: "stack-1", HeadRepositoryID: "11"}) {
		t.Fatal("same-project downstream child was not accepted")
	}
	if isDownstreamChild(parent, &graph.PullRequest{RepositoryID: "99", BaseRefName: "stack-1"}) {
		t.Fatal("fork downstream child was accepted")
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
