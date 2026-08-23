package demo

import (
	"context"
	"strings"
	"time"

	"github.com/ota-takeru/glab-mr-graph/internal/graph"
)

type Loader struct{}

func New() *Loader { return &Loader{} }

func (l *Loader) Load(ctx context.Context, options graph.SearchOptions) (graph.Result, error) {
	return l.LoadProgress(ctx, options, nil)
}

func (l *Loader) LoadProgress(_ context.Context, options graph.SearchOptions, progress func(int, int, string, int)) (graph.Result, error) {
	all := pullRequests()
	selected := map[string]bool{}
	if options.Authored {
		selected["authored"] = true
	}
	if options.Assigned {
		selected["assigned-bot"] = true
	}
	if options.ReviewRequested {
		selected["review-root"] = true
		selected["review-child"] = true
		selected["other-child"] = true
	}
	if strings.TrimSpace(options.Query) != "" {
		for _, pr := range all {
			selected[pr.ID] = true
		}
	}
	prs := make([]*graph.PullRequest, 0, len(selected))
	for _, pr := range all {
		if selected[pr.ID] {
			copy := *pr
			prs = append(prs, &copy)
		}
	}
	if progress != nil {
		progress(1, 1, "Searching merge requests", len(prs))
		progress(1, 1, "Discovering stacked merge requests", len(prs))
	}
	return graph.Build(prs, nil), nil
}

func pullRequests() []*graph.PullRequest {
	updated := time.Date(2026, time.August, 3, 10, 0, 0, 0, time.UTC)
	user := graph.User{Login: "ota-takeru"}
	prs := []*graph.PullRequest{
		{ID: "authored", Number: 104, Title: "Ship the new command palette", URL: "https://gitlab.com/acme/atlas/-/merge_requests/104", UpdatedAt: updated, Author: user, RepositoryID: "atlas", Repository: "acme/atlas", RepositoryURL: "https://gitlab.com/acme/atlas", DefaultBranch: "main", BaseRefName: "release/2026-q3", HeadRefName: "command-palette", HeadRepositoryID: "atlas", HeadRepository: "acme/atlas", ReviewDecision: "APPROVED", ReviewApproved: 2, ReviewTotal: 2, CIState: "SUCCESS", Mergeable: "MERGEABLE", Relation: "mine", Source: "search"},
		{ID: "review-root", Number: 217, Title: "Introduce the agent workflow engine", URL: "https://gitlab.com/acme/atlas/-/merge_requests/217", UpdatedAt: updated, Author: graph.User{Login: "maya"}, RepositoryID: "atlas", Repository: "acme/atlas", RepositoryURL: "https://gitlab.com/acme/atlas", DefaultBranch: "main", BaseRefName: "main", HeadRefName: "agent-workflows", HeadRepositoryID: "atlas", HeadRepository: "acme/atlas", ReviewApproved: 1, ReviewTotal: 3, CIState: "SUCCESS", Mergeable: "MERGEABLE", Relation: "review-requested", Source: "search"},
		{ID: "review-child", Number: 221, Title: "Add parallel tool execution", URL: "https://gitlab.com/acme/atlas/-/merge_requests/221", UpdatedAt: updated, Author: graph.User{Login: "leo"}, RepositoryID: "atlas", Repository: "acme/atlas", RepositoryURL: "https://gitlab.com/acme/atlas", DefaultBranch: "main", BaseRefName: "agent-workflows", HeadRefName: "parallel-tools", HeadRepositoryID: "atlas", HeadRepository: "acme/atlas", ReviewApproved: 0, ReviewTotal: 2, CIState: "PENDING", Mergeable: "MERGEABLE", Relation: "review-requested", Source: "downstream"},
		{ID: "other-child", Number: 223, Title: "Persist agent execution history", URL: "https://gitlab.com/acme/atlas/-/merge_requests/223", IsDraft: true, UpdatedAt: updated, Author: graph.User{Login: "nora"}, RepositoryID: "atlas", Repository: "acme/atlas", RepositoryURL: "https://gitlab.com/acme/atlas", DefaultBranch: "main", BaseRefName: "agent-workflows", HeadRefName: "execution-history", HeadRepositoryID: "atlas", HeadRepository: "acme/atlas", ReviewApproved: 0, ReviewTotal: 1, CIState: "FAILURE", Mergeable: "CONFLICTING", Relation: "other", Source: "downstream"},
		{ID: "assigned-bot", Number: 73, Title: "Bump OpenTelemetry dependencies", URL: "https://gitlab.com/acme/beacon/-/merge_requests/73", UpdatedAt: updated, Author: graph.User{Login: "dependabot[bot]"}, IsBot: true, RepositoryID: "beacon", Repository: "acme/beacon", RepositoryURL: "https://gitlab.com/acme/beacon", DefaultBranch: "main", BaseRefName: "main", HeadRefName: "dependabot/go-modules/otel", HeadRepositoryID: "beacon", HeadRepository: "acme/beacon", Assignees: []graph.User{user}, ReviewApproved: 0, ReviewTotal: 1, CIState: "SUCCESS", Mergeable: "MERGEABLE", Relation: "assigned", Source: "search"},
	}
	for _, pr := range prs {
		pr.Provider = "gitlab"
		pr.BaseCommitSHA = "demo-base-" + pr.ID
		pr.HeadCommitSHA = "demo-head-" + pr.ID
	}
	return prs
}
