// Package gitlab loads merge requests through the authenticated glab CLI.
//
// The client deliberately invokes glab instead of handling tokens itself.  In
// addition to keeping authentication in one place, this makes the client work
// with both GitLab.com and self-managed GitLab installations without writing a
// token or an API response to disk.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ota-takeru/glab-mr-graph/internal/graph"
	"github.com/ota-takeru/glab-mr-graph/internal/oteltrace"
)

const (
	maxConcurrentGitLabRequests = 6
)

// The graph and request limits are intentionally conservative. A malformed or
// very large stack must not turn a refresh into an unbounded API walk.
const (
	defaultMaxMRs         = 500
	defaultMaxDepth       = 20
	defaultMaxRequests    = 1200
	defaultRequestTimeout = 30 * time.Second
	defaultRefreshTimeout = 2 * time.Minute
	mergeRequestsPerPage  = 100
)

// ErrRequestBudgetExceeded identifies a refresh that reached its subprocess
// budget. Callers can use errors.Is when they need to distinguish this from an
// API error returned by glab.
var ErrRequestBudgetExceeded = errors.New("GitLab request budget exceeded")

// CommandRunner is the small seam between the API client and the glab
// executable.  Tests can provide a fixture runner without starting a process.
type CommandRunner interface {
	Run(context.Context, []string) ([]byte, error)
}

// RunnerFunc adapts a function to CommandRunner.
type RunnerFunc func(context.Context, []string) ([]byte, error)

func (f RunnerFunc) Run(ctx context.Context, args []string) ([]byte, error) { return f(ctx, args) }

type execRunner struct{}

func (execRunner) Run(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "glab", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// Do not return stderr verbatim.  Some glab versions include request
		// details in errors, and those details can contain private project data.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("glab api failed: %w", err)
	}
	return out, nil
}

// Client loads GitLab merge requests and converts them to the existing graph
// DTO.  The graph package intentionally retains its historical PullRequest
// type so the rendering and layout code can be shared safely.
type Client struct {
	Hostname       string
	MaxMRs         int
	MaxDepth       int
	RequestTimeout time.Duration
	RefreshTimeout time.Duration
	MaxRequests    int
	Runner         CommandRunner
	Tracer         oteltrace.Tracer

	projectMu    sync.Mutex
	projectCache map[int]*rawProject
	projectCalls map[int]*projectCall
}

type projectCall struct {
	done    chan struct{}
	project *rawProject
	err     error
}

func New(hostname string) *Client {
	return &Client{
		Hostname:       hostname,
		MaxMRs:         defaultMaxMRs,
		MaxDepth:       defaultMaxDepth,
		RequestTimeout: defaultRequestTimeout,
		RefreshTimeout: defaultRefreshTimeout,
		MaxRequests:    defaultMaxRequests,
		Runner:         execRunner{},
		projectCache:   make(map[int]*rawProject),
		projectCalls:   make(map[int]*projectCall),
	}
}

type requestBudgetContextKey struct{}

type requestBudget struct {
	mu    sync.Mutex
	used  int
	limit int
}

func newRequestBudget(limit int) *requestBudget {
	return &requestBudget{limit: limit}
}

func (b *requestBudget) acquire() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used >= b.limit {
		return false
	}
	b.used++
	return true
}

type rawUser struct {
	ID        int    `json:"id"`
	Username  string `json:"username"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
	WebURL    string `json:"web_url"`
	Bot       bool   `json:"bot"`
}

type rawProject struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	WebURL            string `json:"web_url"`
	DefaultBranch     string `json:"default_branch"`
}

type rawPipeline struct {
	Status string `json:"status"`
}

type rawApproval struct {
	ApprovalsRequired int  `json:"approvals_required"`
	ApprovalsLeft     int  `json:"approvals_left"`
	Approved          bool `json:"approved"`
	ApprovedBy        []struct {
		User rawUser `json:"user"`
	} `json:"approved_by"`
}

type rawMergeRequest struct {
	ID                  int          `json:"id"`
	IID                 int          `json:"iid"`
	Title               string       `json:"title"`
	Description         string       `json:"description"`
	WebURL              string       `json:"web_url"`
	State               string       `json:"state"`
	Draft               bool         `json:"draft"`
	WorkInProgress      bool         `json:"work_in_progress"`
	CreatedAt           time.Time    `json:"created_at"`
	UpdatedAt           time.Time    `json:"updated_at"`
	Author              *rawUser     `json:"author"`
	Assignees           []rawUser    `json:"assignees"`
	Reviewers           []rawUser    `json:"reviewers"`
	SourceProjectID     int          `json:"source_project_id"`
	TargetProjectID     int          `json:"target_project_id"`
	SourceBranch        string       `json:"source_branch"`
	TargetBranch        string       `json:"target_branch"`
	DetailedMergeStatus string       `json:"detailed_merge_status"`
	MergeStatus         string       `json:"merge_status"`
	HasConflicts        bool         `json:"has_conflicts"`
	HeadPipeline        *rawPipeline `json:"head_pipeline"`
	Pipeline            *rawPipeline `json:"pipeline"`
	SourceProject       *rawProject  `json:"source_project"`
	TargetProject       *rawProject  `json:"target_project"`
}

type searchSpec struct {
	scope string
	query string
}

type searchResult struct {
	items     []rawMergeRequest
	truncated bool
	err       error
}

// buildSearchSpecs maps the three relationship filters to GitLab's global
// merge-request scopes.  The optional text search is intentionally a fourth
// OR branch, matching the UI semantics of the original graph.
func buildSearchSpecs(options graph.SearchOptions) []searchSpec {
	result := make([]searchSpec, 0, 4)
	if options.Authored {
		result = append(result, searchSpec{scope: "created_by_me"})
	}
	if options.Assigned {
		result = append(result, searchSpec{scope: "assigned_to_me"})
	}
	if options.ReviewRequested {
		result = append(result, searchSpec{scope: "reviews_for_me"})
	}
	if query := strings.TrimSpace(options.Query); query != "" {
		result = append(result, searchSpec{scope: "all", query: query})
	}
	return result
}

func (c *Client) searchMergeRequests(ctx context.Context, specs []searchSpec) []searchResult {
	results := make([]searchResult, len(specs))
	workers := min(len(specs), maxConcurrentGitLabRequests)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				items, truncated, err := c.listMergeRequests(ctx, specs[index], c.maxMRs())
				results[index] = searchResult{items: items, truncated: truncated, err: err}
			}
		}()
	}
	for index := range specs {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return results
}

// Load implements server.Loader.
func (c *Client) Load(ctx context.Context, options graph.SearchOptions) (graph.Result, error) {
	return c.LoadProgress(ctx, options, nil)
}

// LoadProgress searches, hydrates and discovers a bounded merge-request graph.
func (c *Client) LoadProgress(ctx context.Context, options graph.SearchOptions, progress func(current, total int, phase string, collected int)) (result graph.Result, resultErr error) {
	return c.loadStages(ctx, options, progress, nil)
}

// LoadStages emits the topology before the slower detail and approval requests
// finish, then emits the status-rich graph as the complete stage.
func (c *Client) LoadStages(ctx context.Context, options graph.SearchOptions, progress func(current, total int, phase string, collected int), emit func(stage string, result graph.Result)) (result graph.Result, resultErr error) {
	return c.loadStages(ctx, options, progress, emit)
}

func (c *Client) loadStages(ctx context.Context, options graph.SearchOptions, progress func(current, total int, phase string, collected int), emit func(stage string, result graph.Result)) (result graph.Result, resultErr error) {
	refreshCtx, cancel := context.WithTimeout(ctx, c.refreshTimeout())
	defer cancel()
	ctx = context.WithValue(refreshCtx, requestBudgetContextKey{}, newRequestBudget(c.maxRequests()))
	ctx, span := c.startSpan(ctx, "load merge request graph", oteltrace.SpanInternal, oteltrace.Attributes{"mr.has_search_query": options.Query != ""})
	if span != nil {
		defer func() {
			span.End(resultErr, oteltrace.Attributes{"mr.node_count": len(result.Nodes), "mr.edge_count": len(result.Edges)})
		}()
	}
	report := func(current, total int, phase string, collected int) {
		if progress != nil {
			progress(current, total, phase, collected)
		}
	}
	specs := buildSearchSpecs(options)
	var viewer rawUser
	var viewerErr error
	viewerDone := make(chan struct{})
	go func() {
		viewer, viewerErr = c.currentUser(ctx)
		close(viewerDone)
	}()
	if len(specs) == 0 {
		<-viewerDone
		if viewerErr != nil {
			return graph.Result{}, viewerErr
		}
		result = graph.Build(nil, nil)
		if emit != nil {
			emit("topology", result)
			emit("complete", result)
		}
		return result, nil
	}
	report(0, len(specs), "Searching merge requests", 0)
	rawByID := make(map[string]rawMergeRequest)
	sourceByID := make(map[string]string)
	directReview := make(map[string]bool)
	warnings := make([]string, 0)
	searchLimitReached := false
	searchResults := c.searchMergeRequests(ctx, specs)
	<-viewerDone
	if viewerErr != nil {
		return graph.Result{}, viewerErr
	}
	for i, result := range searchResults {
		if result.err != nil {
			return graph.Result{}, result.err
		}
		searchLimitReached = searchLimitReached || result.truncated
		for _, raw := range result.items {
			if raw.State != "" && raw.State != "opened" {
				continue
			}
			id := rawMRID(raw)
			if id == "" {
				continue
			}
			if _, exists := rawByID[id]; !exists && len(rawByID) >= c.maxMRs() {
				searchLimitReached = true
				break
			}
			if specs[i].scope == "reviews_for_me" {
				directReview[id] = true
			}
			if _, exists := rawByID[id]; !exists {
				rawByID[id] = raw
				sourceByID[id] = "search"
			}
		}
		report(i+1, len(specs), "Searching merge requests", len(rawByID))
	}

	searchItems := make([]rawMergeRequest, 0, len(rawByID))
	for _, raw := range rawByID {
		searchItems = append(searchItems, raw)
	}
	sort.Slice(searchItems, func(i, j int) bool { return rawMRID(searchItems[i]) < rawMRID(searchItems[j]) })
	searchItems, projectWarnings, _ := c.enrichProjectsMany(ctx, searchItems)
	warnings = append(warnings, projectWarnings...)
	for _, raw := range searchItems {
		rawByID[rawMRID(raw)] = raw
	}

	seedIDs := make([]string, 0, len(rawByID))
	for id := range rawByID {
		seedIDs = append(seedIDs, id)
	}
	sort.Strings(seedIDs)
	report(0, len(seedIDs), "Discovering stacked merge requests", len(rawByID))
	discovered, discoveryWarnings := c.discoverRawStacks(ctx, seedIDs, rawByID, sourceByID, viewer.Username, report)
	warnings = append(warnings, discoveryWarnings...)
	if searchLimitReached {
		warnings = append(warnings, "Merge request limit reached; narrow the search to see the complete graph.")
	}
	if discovered {
		warnings = append(warnings, "Merge request limit reached; narrow the search to see the complete graph.")
	}
	warnings = uniqueWarnings(warnings)
	topologyPRs := c.convertMany(rawByID, nil, sourceByID, directReview, viewer.Username, true)
	topology := graph.Build(topologyPRs, warnings)
	if emit != nil {
		emit("topology", topology)
	}
	report(0, len(rawByID), "Loading merge request status", len(rawByID))

	allItems := make([]rawMergeRequest, 0, len(rawByID))
	for _, raw := range rawByID {
		allItems = append(allItems, raw)
	}
	sort.Slice(allItems, func(i, j int) bool { return rawMRID(allItems[i]) < rawMRID(allItems[j]) })
	hydrated, hydrateWarnings, _ := c.hydrateManyDetailed(ctx, allItems)
	warnings = uniqueWarnings(append(warnings, hydrateWarnings...))
	finalRaw := make(map[string]rawMergeRequest, len(hydrated))
	approvals := make(map[string]*rawApproval, len(hydrated))
	for _, item := range hydrated {
		id := rawMRID(item.mr)
		finalRaw[id] = item.mr
		approvals[id] = item.approval
	}
	prs := c.convertMany(finalRaw, approvals, sourceByID, directReview, viewer.Username, false)
	result = graph.Build(prs, warnings)
	report(len(rawByID), len(rawByID), "Loading merge request status", len(rawByID))
	if emit != nil {
		emit("complete", result)
	}
	return result, nil
}

func (c *Client) convertMany(rawByID map[string]rawMergeRequest, approvals map[string]*rawApproval, sourceByID map[string]string, directReview map[string]bool, viewer string, loading bool) []*graph.PullRequest {
	ids := make([]string, 0, len(rawByID))
	for id := range rawByID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	prs := make([]*graph.PullRequest, 0, len(ids))
	for _, id := range ids {
		raw := rawByID[id]
		pr := c.convert(raw, approvals[id], viewer, sourceByID[id])
		if pr == nil {
			continue
		}
		if loading {
			pr.ApprovalState = "LOADING"
			pr.ApprovalRequired = 0
			pr.ApprovalRemaining = 0
			pr.ApproverCount = 0
			pr.ReviewApproved = 0
			pr.ReviewTotal = 0
			pr.ReviewDecision = ""
		}
		pr.Relation = graph.RelationFor(pr, viewer, directReview[id] || hasReviewer(raw, viewer))
		prs = append(prs, pr)
	}
	return prs
}

type hydratedMR struct {
	mr       rawMergeRequest
	approval *rawApproval
}

func (c *Client) hydrateMany(ctx context.Context, items []rawMergeRequest) ([]hydratedMR, []string) {
	ordered, warnings, _ := c.hydrateManyDetailed(ctx, items)
	return ordered, warnings
}

func (c *Client) hydrateManyDetailed(ctx context.Context, items []rawMergeRequest) ([]hydratedMR, []string, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	workers := maxConcurrentGitLabRequests
	if len(items) < workers {
		workers = len(items)
	}
	type job struct {
		index int
		mr    rawMergeRequest
	}
	type response struct {
		index       int
		mr          rawMergeRequest
		approval    *rawApproval
		detailErr   error
		approvalErr error
	}
	jobs := make(chan job)
	responses := make(chan response, len(items))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				detail, approval, detailErr, approvalErr := c.hydrateOne(ctx, item.mr)
				responses <- response{index: item.index, mr: detail, approval: approval, detailErr: detailErr, approvalErr: approvalErr}
			}
		}()
	}
	go func() {
		for i, mr := range items {
			jobs <- job{index: i, mr: mr}
		}
		close(jobs)
		wg.Wait()
		close(responses)
	}()
	orderedResponses := make([]response, len(items))
	for item := range responses {
		orderedResponses[item.index] = item
	}
	ordered := make([]hydratedMR, len(items))
	warnings := make([]string, 0)
	var stopDiscoveryErr error
	for _, item := range orderedResponses {
		ordered[item.index] = hydratedMR{mr: item.mr, approval: item.approval}
		if item.detailErr != nil {
			if stopDiscoveryErr == nil && c.shouldStopDiscovery(item.detailErr) {
				stopDiscoveryErr = item.detailErr
			}
			warnings = append(warnings, "Some GitLab merge request details could not be loaded; list data was retained.")
		}
		if item.approvalErr != nil {
			if stopDiscoveryErr == nil && c.shouldStopDiscovery(item.approvalErr) {
				stopDiscoveryErr = item.approvalErr
			}
			// Approval rules are optional (and unavailable on some tiers). Keep
			// the MR visible and expose the unavailable state in the DTO.
			warnings = append(warnings, "Some GitLab approval data is unavailable; approval counts may be unavailable.")
		}
	}
	return ordered, uniqueWarnings(warnings), stopDiscoveryErr
}

func (c *Client) hydrateOne(ctx context.Context, mr rawMergeRequest) (rawMergeRequest, *rawApproval, error, error) {
	detail := mr
	var detailErr error
	if mr.TargetProjectID > 0 && mr.IID > 0 {
		var fetched rawMergeRequest
		detailErr = c.apiJSON(ctx, "get merge request detail", mergeRequestPath(mr.TargetProjectID, mr.IID), &fetched)
		if detailErr == nil && fetched.IID != 0 {
			detail = mergeRequestOverlay(mr, fetched)
		}
	}
	// The list endpoint supplies project IDs but not a stable namespace or
	// default branch on all GitLab versions. Project lookups are cached in
	// memory and are best-effort so a missing project response never hides an
	// otherwise usable merge request.
	if detail.TargetProject == nil {
		if project, err := c.project(ctx, detail.TargetProjectID); err == nil {
			detail.TargetProject = project
		}
	}
	if detail.SourceProject == nil && detail.SourceProjectID != detail.TargetProjectID {
		if project, err := c.project(ctx, detail.SourceProjectID); err == nil {
			detail.SourceProject = project
		}
	}
	var approval rawApproval
	var approvalErr error
	if detail.TargetProjectID > 0 && detail.IID > 0 {
		approvalErr = c.apiJSON(ctx, "get merge request approvals", approvalPath(detail.TargetProjectID, detail.IID), &approval)
	}
	if approvalErr != nil {
		return detail, nil, detailErr, approvalErr
	}
	return detail, &approval, detailErr, nil
}

func mergeRequestOverlay(base, detail rawMergeRequest) rawMergeRequest {
	// Detail responses are generally richer, but older GitLab versions omit a
	// few fields. Keep list values when a detail field is empty.
	detail.Draft = detail.Draft || base.Draft
	detail.WorkInProgress = detail.WorkInProgress || base.WorkInProgress
	detail.HasConflicts = detail.HasConflicts || base.HasConflicts
	if detail.ID == 0 {
		detail.ID = base.ID
	}
	if detail.IID == 0 {
		detail.IID = base.IID
	}
	if detail.TargetProjectID == 0 {
		detail.TargetProjectID = base.TargetProjectID
	}
	if detail.SourceProjectID == 0 {
		detail.SourceProjectID = base.SourceProjectID
	}
	if detail.Title == "" {
		detail.Title = base.Title
	}
	if detail.WebURL == "" {
		detail.WebURL = base.WebURL
	}
	if detail.SourceBranch == "" {
		detail.SourceBranch = base.SourceBranch
	}
	if detail.TargetBranch == "" {
		detail.TargetBranch = base.TargetBranch
	}
	if detail.UpdatedAt.IsZero() {
		detail.UpdatedAt = base.UpdatedAt
	}
	if detail.Author == nil {
		detail.Author = base.Author
	}
	if len(detail.Assignees) == 0 {
		detail.Assignees = base.Assignees
	}
	if len(detail.Reviewers) == 0 {
		detail.Reviewers = base.Reviewers
	}
	if detail.HeadPipeline == nil {
		detail.HeadPipeline = base.HeadPipeline
	}
	if detail.Pipeline == nil {
		detail.Pipeline = base.Pipeline
	}
	if detail.SourceProject == nil {
		detail.SourceProject = base.SourceProject
	}
	if detail.TargetProject == nil {
		detail.TargetProject = base.TargetProject
	}
	if detail.DetailedMergeStatus == "" {
		detail.DetailedMergeStatus = base.DetailedMergeStatus
	}
	if detail.MergeStatus == "" {
		detail.MergeStatus = base.MergeStatus
	}
	return detail
}

type stackDirection uint8

const (
	stackBoth stackDirection = iota
	stackUpstream
	stackDownstream
)

type stackItem struct {
	pr        *graph.PullRequest
	depth     int
	direction stackDirection
}

type branchJob struct {
	search    branchSearch
	current   *graph.PullRequest
	direction stackDirection
	depth     int
}

type branchResult struct {
	items     []rawMergeRequest
	truncated bool
	err       error
}

type discoveredCandidate struct {
	raw       rawMergeRequest
	current   *graph.PullRequest
	direction stackDirection
	depth     int
}

func (c *Client) enrichProjectsMany(ctx context.Context, items []rawMergeRequest) ([]rawMergeRequest, []string, error) {
	if len(items) == 0 {
		return nil, nil, nil
	}
	workers := min(len(items), maxConcurrentGitLabRequests)
	type response struct {
		index int
		mr    rawMergeRequest
		err   error
	}
	jobs := make(chan int)
	responses := make(chan response, len(items))
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				mr := items[index]
				var firstErr error
				if mr.TargetProject == nil {
					project, err := c.project(ctx, mr.TargetProjectID)
					if err != nil {
						firstErr = err
					} else {
						mr.TargetProject = project
					}
				}
				if mr.SourceProject == nil && mr.SourceProjectID != mr.TargetProjectID {
					project, err := c.project(ctx, mr.SourceProjectID)
					if err != nil && firstErr == nil {
						firstErr = err
					} else if err == nil {
						mr.SourceProject = project
					}
				}
				responses <- response{index: index, mr: mr, err: firstErr}
			}
		}()
	}
	go func() {
		for index := range items {
			jobs <- index
		}
		close(jobs)
		wg.Wait()
		close(responses)
	}()
	ordered := make([]rawMergeRequest, len(items))
	warnings := make([]string, 0)
	var stopErr error
	for response := range responses {
		ordered[response.index] = response.mr
		if response.err != nil {
			warnings = append(warnings, "Some GitLab project metadata could not be loaded; fallback project names were retained.")
			if stopErr == nil && c.shouldStopDiscovery(response.err) {
				stopErr = response.err
			}
		}
	}
	return ordered, uniqueWarnings(warnings), stopErr
}

// discoverRawStacks resolves stack identity using list responses and project
// metadata only. Detail and approval requests are deliberately deferred until
// after the topology stage has been emitted.
func (c *Client) discoverRawStacks(ctx context.Context, seedIDs []string, rawByID map[string]rawMergeRequest, sourceByID map[string]string, viewer string, report func(int, int, string, int)) (bool, []string) {
	frontier := make([]stackItem, 0, len(seedIDs))
	for _, id := range seedIDs {
		pr := c.convert(rawByID[id], nil, viewer, sourceByID[id])
		if pr != nil {
			frontier = append(frontier, stackItem{pr: pr, direction: stackBoth})
		}
	}
	visitedDownstream := make(map[string]bool)
	visitedUpstream := make(map[string]bool)
	warnings := make([]string, 0)
	limitReached := false
	stopDiscovery := false
	processed := 0

	for len(frontier) > 0 && len(rawByID) < c.maxMRs() && !stopDiscovery {
		jobs := make([]branchJob, 0, len(frontier)*2)
		for _, current := range frontier {
			processed++
			if current.depth >= c.maxDepth() {
				continue
			}
			if (current.direction == stackBoth || current.direction == stackDownstream) && current.pr.HeadRepositoryID != "" && current.pr.HeadRefName != "" {
				key := refKey(current.pr.HeadRepositoryID, current.pr.HeadRefName)
				if !visitedDownstream[key] {
					visitedDownstream[key] = true
					jobs = append(jobs, branchJob{search: branchSearch{projectID: current.pr.HeadRepositoryID, targetBranch: current.pr.HeadRefName}, current: current.pr, direction: stackDownstream, depth: current.depth + 1})
				}
			}
			if (current.direction == stackBoth || current.direction == stackUpstream) && current.pr.RepositoryID != "" && current.pr.BaseRefName != "" && current.pr.DefaultBranch != "" && current.pr.BaseRefName != current.pr.DefaultBranch {
				key := refKey(current.pr.RepositoryID, current.pr.BaseRefName)
				if !visitedUpstream[key] {
					visitedUpstream[key] = true
					jobs = append(jobs, branchJob{search: branchSearch{projectID: current.pr.RepositoryID, sourceBranch: current.pr.BaseRefName}, current: current.pr, direction: stackUpstream, depth: current.depth + 1})
				}
			}
		}
		if len(jobs) == 0 {
			break
		}
		remaining := c.maxMRs() - len(rawByID)
		results := c.listBranchJobs(ctx, jobs, remaining)
		candidates := make([]discoveredCandidate, 0)
		candidateByID := make(map[string]bool)
		for index, result := range results {
			job := jobs[index]
			if result.truncated {
				limitReached = true
			}
			if result.err != nil {
				if c.shouldStopDiscovery(result.err) {
					warnings = append(warnings, c.discoveryStopWarning(result.err))
					stopDiscovery = true
				} else if job.direction == stackDownstream {
					warnings = append(warnings, "Could not discover downstream GitLab merge requests for a stack branch.")
				} else {
					warnings = append(warnings, "Could not discover upstream GitLab merge requests for a stack branch.")
				}
				continue
			}
			for _, raw := range result.items {
				id := rawMRID(raw)
				_, exists := rawByID[id]
				if id == "" || id == job.current.ID || exists || candidateByID[id] || !matchesBranchCandidate(raw, job.current, job.direction) {
					continue
				}
				if len(candidates) >= remaining {
					limitReached = true
					continue
				}
				candidateByID[id] = true
				candidates = append(candidates, discoveredCandidate{raw: raw, current: job.current, direction: job.direction, depth: job.depth})
			}
		}
		if len(candidates) == 0 {
			frontier = nil
			continue
		}
		rawItems := make([]rawMergeRequest, len(candidates))
		for i, candidate := range candidates {
			rawItems[i] = candidate.raw
		}
		enriched, projectWarnings, projectStopErr := c.enrichProjectsMany(ctx, rawItems)
		warnings = append(warnings, projectWarnings...)
		if projectStopErr != nil {
			warnings = append(warnings, c.discoveryStopWarning(projectStopErr))
			stopDiscovery = true
		}
		next := make([]stackItem, 0, len(enriched))
		for i, raw := range enriched {
			if len(rawByID) >= c.maxMRs() {
				limitReached = true
				break
			}
			candidate := candidates[i]
			id := rawMRID(raw)
			if _, exists := rawByID[id]; id == "" || exists {
				continue
			}
			pr := c.convert(raw, nil, viewer, stackSource(candidate.direction))
			if pr == nil {
				continue
			}
			if candidate.direction == stackDownstream {
				if !isDownstreamChild(candidate.current, pr) {
					continue
				}
			} else if !isUpstreamParent(pr, candidate.current) {
				continue
			}
			rawByID[id] = raw
			sourceByID[id] = stackSource(candidate.direction)
			next = append(next, stackItem{pr: pr, depth: candidate.depth, direction: candidate.direction})
		}
		if len(next) > 0 && report != nil {
			report(processed, max(1, len(seedIDs)), "Discovering stacked merge requests", len(rawByID))
		}
		frontier = next
	}
	if len(rawByID) >= c.maxMRs() {
		limitReached = true
	}
	return limitReached, uniqueWarnings(warnings)
}

func (c *Client) discoverStacks(ctx context.Context, seeds []*graph.PullRequest, byID map[string]*graph.PullRequest, viewer string, report func(int, int, string, int)) (bool, []string) {
	frontier := make([]stackItem, 0, len(seeds))
	for _, seed := range seeds {
		frontier = append(frontier, stackItem{pr: seed, direction: stackBoth})
	}
	visitedDownstream := make(map[string]bool)
	visitedUpstream := make(map[string]bool)
	warnings := make([]string, 0)
	limitReached := false
	stopDiscovery := false
	processed := 0

	// Direct search seeds can be explored in both directions. A node discovered
	// upstream or downstream retains that direction, so an upstream parent
	// cannot make the traversal fan out into its unrelated downstream siblings.
	for len(frontier) > 0 && len(byID) < c.maxMRs() && !stopDiscovery {
		jobs := make([]branchJob, 0, len(frontier)*2)
		for _, current := range frontier {
			processed++
			if current.depth >= c.maxDepth() {
				continue
			}
			if (current.direction == stackBoth || current.direction == stackDownstream) && current.pr.HeadRepositoryID != "" && current.pr.HeadRefName != "" {
				key := refKey(current.pr.HeadRepositoryID, current.pr.HeadRefName)
				if !visitedDownstream[key] {
					visitedDownstream[key] = true
					jobs = append(jobs, branchJob{
						search:  branchSearch{projectID: current.pr.HeadRepositoryID, targetBranch: current.pr.HeadRefName},
						current: current.pr, direction: stackDownstream, depth: current.depth + 1,
					})
				}
			}
			if (current.direction == stackBoth || current.direction == stackUpstream) && current.pr.RepositoryID != "" && current.pr.BaseRefName != "" && current.pr.BaseRefName != current.pr.DefaultBranch {
				key := refKey(current.pr.RepositoryID, current.pr.BaseRefName)
				if !visitedUpstream[key] {
					visitedUpstream[key] = true
					jobs = append(jobs, branchJob{
						search:  branchSearch{projectID: current.pr.RepositoryID, sourceBranch: current.pr.BaseRefName},
						current: current.pr, direction: stackUpstream, depth: current.depth + 1,
					})
				}
			}
		}
		if len(jobs) == 0 {
			break
		}
		remaining := c.maxMRs() - len(byID)
		results := c.listBranchJobs(ctx, jobs, remaining)
		candidates := make([]discoveredCandidate, 0)
		candidateByID := make(map[string]bool)
		for index, result := range results {
			job := jobs[index]
			if result.truncated {
				limitReached = true
			}
			if result.err != nil {
				if c.shouldStopDiscovery(result.err) {
					warnings = append(warnings, c.discoveryStopWarning(result.err))
					stopDiscovery = true
				} else if job.direction == stackDownstream {
					warnings = append(warnings, "Could not discover downstream GitLab merge requests for a stack branch.")
				} else {
					warnings = append(warnings, "Could not discover upstream GitLab merge requests for a stack branch.")
				}
				continue
			}
			for _, raw := range result.items {
				id := rawMRID(raw)
				if id == "" || id == job.current.ID || byID[id] != nil || candidateByID[id] {
					continue
				}
				if !matchesBranchCandidate(raw, job.current, job.direction) {
					continue
				}
				if len(candidates) >= remaining {
					limitReached = true
					continue
				}
				candidateByID[id] = true
				candidates = append(candidates, discoveredCandidate{raw: raw, current: job.current, direction: job.direction, depth: job.depth})
			}
		}

		if len(candidates) > 0 {
			rawItems := make([]rawMergeRequest, len(candidates))
			for i, candidate := range candidates {
				rawItems[i] = candidate.raw
			}
			hydrated, hydrationWarnings, hydrationStopErr := c.hydrateManyDetailed(ctx, rawItems)
			warnings = append(warnings, hydrationWarnings...)
			if hydrationStopErr != nil {
				warnings = append(warnings, c.discoveryStopWarning(hydrationStopErr))
				stopDiscovery = true
			}
			next := make([]stackItem, 0, len(hydrated))
			added := 0
			for i, item := range hydrated {
				if len(byID) >= c.maxMRs() {
					limitReached = true
					break
				}
				candidate := candidates[i]
				id := rawMRID(item.mr)
				if id == "" || byID[id] != nil {
					continue
				}
				pr := c.convert(item.mr, item.approval, viewer, stackSource(candidate.direction))
				if pr == nil {
					continue
				}
				if candidate.direction == stackDownstream {
					if !isDownstreamChild(candidate.current, pr) {
						continue
					}
				} else if !isUpstreamParent(pr, candidate.current) {
					continue
				}
				pr.Relation = graph.RelationFor(pr, viewer, hasReviewer(item.mr, viewer))
				byID[id] = pr
				next = append(next, stackItem{pr: pr, depth: candidate.depth, direction: candidate.direction})
				added++
			}
			if added > 0 && report != nil {
				report(processed, max(1, len(seeds)), "Discovering stacked merge requests", len(byID))
			}
			frontier = next
			continue
		}
		frontier = nil
	}
	if len(byID) >= c.maxMRs() {
		limitReached = true
	}
	return limitReached, uniqueWarnings(warnings)
}

type branchSearch struct {
	projectID    string
	sourceBranch string
	targetBranch string
}

func (c *Client) listBranchJobs(ctx context.Context, jobs []branchJob, limit int) []branchResult {
	results := make([]branchResult, len(jobs))
	if len(jobs) == 0 || limit <= 0 {
		return results
	}
	workers := min(len(jobs), maxConcurrentGitLabRequests)
	indexes := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range indexes {
				items, truncated, err := c.listBranchMergeRequests(ctx, jobs[index].search, limit)
				results[index] = branchResult{items: items, truncated: truncated, err: err}
			}
		}()
	}
	for index := range jobs {
		indexes <- index
	}
	close(indexes)
	wg.Wait()
	return results
}

func matchesBranchCandidate(raw rawMergeRequest, current *graph.PullRequest, direction stackDirection) bool {
	if direction == stackDownstream {
		return strconv.Itoa(raw.TargetProjectID) == current.HeadRepositoryID && raw.TargetBranch == current.HeadRefName
	}
	return strconv.Itoa(raw.SourceProjectID) == current.RepositoryID && raw.SourceBranch == current.BaseRefName
}

func stackSource(direction stackDirection) string {
	if direction == stackDownstream {
		return "downstream"
	}
	return "upstream"
}

func (c *Client) maxMRs() int {
	if c.MaxMRs > 0 {
		return c.MaxMRs
	}
	return defaultMaxMRs
}
func (c *Client) maxDepth() int {
	if c.MaxDepth > 0 {
		return c.MaxDepth
	}
	return defaultMaxDepth
}

func (c *Client) maxRequests() int {
	if c.MaxRequests > 0 {
		return c.MaxRequests
	}
	return defaultMaxRequests
}

func (c *Client) requestTimeout() time.Duration {
	if c.RequestTimeout > 0 {
		return c.RequestTimeout
	}
	return defaultRequestTimeout
}

func (c *Client) refreshTimeout() time.Duration {
	if c.RefreshTimeout > 0 {
		return c.RefreshTimeout
	}
	return defaultRefreshTimeout
}

func (c *Client) shouldStopDiscovery(err error) bool {
	return errors.Is(err, ErrRequestBudgetExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (c *Client) discoveryStopWarning(err error) string {
	if errors.Is(err, ErrRequestBudgetExceeded) {
		return "GitLab request budget exhausted; stack discovery stopped."
	}
	return "GitLab refresh timed out or was canceled; stack discovery stopped."
}

func refKey(projectID, branch string) string { return projectID + "\x00" + branch }

func rawMRID(mr rawMergeRequest) string {
	if mr.ID > 0 {
		return "gitlab:mr:" + strconv.Itoa(mr.ID)
	}
	if mr.TargetProjectID > 0 && mr.IID > 0 {
		return fmt.Sprintf("gitlab:mr:%d!%d", mr.TargetProjectID, mr.IID)
	}
	return ""
}

func mergeRequestPath(projectID, iid int) string {
	return fmt.Sprintf("/projects/%d/merge_requests/%d", projectID, iid)
}
func approvalPath(projectID, iid int) string { return mergeRequestPath(projectID, iid) + "/approvals" }

func queryPath(path string, values url.Values) string {
	if len(values) == 0 {
		return path
	}
	return path + "?" + values.Encode()
}

func (c *Client) listMergeRequests(ctx context.Context, spec searchSpec, limit int) ([]rawMergeRequest, bool, error) {
	values := url.Values{"per_page": {strconv.Itoa(mergeRequestsPerPage)}, "scope": {spec.scope}, "state": {"opened"}}
	if spec.query != "" {
		values.Set("search", spec.query)
	}
	return c.listAPI(ctx, "list merge requests", queryPath("/merge_requests", values), limit)
}

func (c *Client) listBranchMergeRequests(ctx context.Context, search branchSearch, limit int) ([]rawMergeRequest, bool, error) {
	values := url.Values{"per_page": {strconv.Itoa(mergeRequestsPerPage)}, "state": {"opened"}}
	path := "/projects/" + url.PathEscape(search.projectID) + "/merge_requests"
	if search.sourceBranch != "" {
		// A child may target a project while its parent targets a different
		// project. Search globally, then validate the source project and branch
		// against the child after decoding the response.
		path = "/merge_requests"
		values.Set("scope", "all")
		values.Set("source_branch", search.sourceBranch)
	}
	if search.targetBranch != "" {
		values.Set("target_branch", search.targetBranch)
	}
	return c.listAPI(ctx, "list stacked merge requests", queryPath(path, values), limit)
}

func (c *Client) listAPI(ctx context.Context, operation, endpoint string, limit int) ([]rawMergeRequest, bool, error) {
	if limit <= 0 {
		return nil, false, nil
	}
	items := make([]rawMergeRequest, 0, min(limit, mergeRequestsPerPage))
	for page := 1; ; page++ {
		pageEndpoint, err := pagedEndpoint(endpoint, page)
		if err != nil {
			return nil, false, fmt.Errorf("build GitLab pagination endpoint: %w", err)
		}
		var pageItems []rawMergeRequest
		if err := c.apiJSON(ctx, operation, pageEndpoint, &pageItems); err != nil {
			return nil, false, err
		}
		remaining := limit - len(items)
		if len(pageItems) >= remaining {
			items = append(items, pageItems[:remaining]...)
			truncated := len(pageItems) > remaining || len(pageItems) == mergeRequestsPerPage
			return items, truncated, nil
		}
		items = append(items, pageItems...)
		if len(pageItems) < mergeRequestsPerPage {
			return items, false, nil
		}
	}
}

func pagedEndpoint(endpoint string, page int) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	values := parsed.Query()
	values.Set("per_page", strconv.Itoa(mergeRequestsPerPage))
	values.Set("page", strconv.Itoa(page))
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func (c *Client) currentUser(ctx context.Context) (rawUser, error) {
	var user rawUser
	if err := c.apiJSON(ctx, "get current GitLab user", "/user", &user); err != nil {
		return rawUser{}, err
	}
	if user.Username == "" {
		return rawUser{}, errors.New("GitLab user response did not contain a username")
	}
	return user, nil
}

func (c *Client) apiJSON(ctx context.Context, operation, endpoint string, target any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if budget, ok := ctx.Value(requestBudgetContextKey{}).(*requestBudget); ok && !budget.acquire() {
		return fmt.Errorf("%w while %s", ErrRequestBudgetExceeded, operation)
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.requestTimeout())
	defer cancel()
	args := []string{"api"}
	if c.Hostname != "" {
		args = append(args, "--hostname", c.Hostname)
	}
	args = append(args, endpoint)
	_, span := c.startSpan(requestCtx, "glab api request", oteltrace.SpanClient, oteltrace.Attributes{
		"process.executable.name": "glab",
		"gitlab.operation.name":   operation,
	})
	out, err := c.runner().Run(requestCtx, args)
	if span != nil {
		span.End(err, nil)
	}
	if err != nil {
		return fmt.Errorf("GitLab API %s: %w", operation, err)
	}
	if err := decodeJSONPages(out, target); err != nil {
		return fmt.Errorf("decode GitLab response: %w", err)
	}
	return nil
}

func (c *Client) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return execRunner{}
}

// decodeJSONPages accepts one JSON value and, for compatibility with older
// fixtures, newline-separated JSON arrays. Manual pagination only supplies one
// page per command invocation.
func decodeJSONPages(data []byte, target any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return errors.New("empty GitLab response")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if list, ok := target.(*[]rawMergeRequest); ok {
		result := make([]rawMergeRequest, 0)
		for {
			var page []rawMergeRequest
			err := decoder.Decode(&page)
			if err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			}
			result = append(result, page...)
		}
		*list = result
		return nil
	}
	return json.Unmarshal(trimmed, target)
}

func uniqueWarnings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func (c *Client) project(ctx context.Context, id int) (*rawProject, error) {
	if id <= 0 {
		return nil, nil
	}
	c.projectMu.Lock()
	if project := c.projectCache[id]; project != nil {
		copy := *project
		c.projectMu.Unlock()
		return &copy, nil
	}
	if c.projectCache == nil {
		c.projectCache = make(map[int]*rawProject)
	}
	if call := c.projectCalls[id]; call != nil {
		c.projectMu.Unlock()
		select {
		case <-call.done:
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if call.project == nil {
				return nil, call.err
			}
			copy := *call.project
			return &copy, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.projectCalls == nil {
		c.projectCalls = make(map[int]*projectCall)
	}
	call := &projectCall{done: make(chan struct{})}
	c.projectCalls[id] = call
	c.projectMu.Unlock()

	var project rawProject
	err := c.apiJSON(ctx, "get GitLab project", "/projects/"+strconv.Itoa(id), &project)
	c.projectMu.Lock()
	call.err = err
	if err == nil {
		cached := project
		call.project = &cached
		c.projectCache[id] = &cached
	}
	delete(c.projectCalls, id)
	close(call.done)
	c.projectMu.Unlock()
	if err != nil {
		return nil, err
	}
	result := project
	return &result, nil
}

func (c *Client) convert(raw rawMergeRequest, approval *rawApproval, viewer, source string) *graph.PullRequest {
	id := rawMRID(raw)
	if id == "" || raw.TargetProjectID <= 0 || raw.IID <= 0 {
		return nil
	}
	target := raw.TargetProject
	if target == nil {
		target = &rawProject{ID: raw.TargetProjectID, PathWithNamespace: fmt.Sprintf("project/%d", raw.TargetProjectID), WebURL: raw.WebURL}
	}
	sourceProject := raw.SourceProject
	if sourceProject == nil {
		if raw.SourceProjectID > 0 && raw.SourceProjectID != raw.TargetProjectID {
			sourceProject = &rawProject{ID: raw.SourceProjectID, PathWithNamespace: fmt.Sprintf("project/%d", raw.SourceProjectID)}
		} else {
			sourceProject = target
		}
	}
	approvalState, approvalRequired, approvalRemaining, approverCount := approvalFields(approval)
	pr := &graph.PullRequest{
		ID: id, Number: raw.IID, Title: raw.Title, URL: raw.WebURL, IsDraft: raw.Draft || raw.WorkInProgress,
		UpdatedAt: raw.UpdatedAt, RepositoryID: strconv.Itoa(raw.TargetProjectID), Repository: target.PathWithNamespace, RepositoryURL: target.WebURL, Provider: "gitlab",
		DefaultBranch: target.DefaultBranch, BaseRefName: raw.TargetBranch, HeadRefName: raw.SourceBranch,
		HeadRepositoryID: strconv.Itoa(raw.SourceProjectID), HeadRepository: sourceProject.PathWithNamespace,
		Assignees: make([]graph.User, 0, len(raw.Assignees)), Relation: "other", Source: source,
		CIState: pipelineState(raw), Mergeable: mergeState(raw), ReviewDecision: approvalDecision(approval),
		ApprovalState: approvalState, ApprovalRequired: approvalRequired, ApprovalRemaining: approvalRemaining, ApproverCount: approverCount,
	}
	if pr.HeadRepositoryID == "0" || pr.HeadRepositoryID == "" {
		pr.HeadRepositoryID = pr.RepositoryID
	}
	if pr.HeadRepository == "" {
		pr.HeadRepository = pr.Repository
	}
	if raw.Author != nil {
		pr.Author = graph.User{Login: gitlabLogin(*raw.Author), AvatarURL: raw.Author.AvatarURL}
		pr.IsBot = raw.Author.Bot || strings.HasSuffix(strings.ToLower(pr.Author.Login), "[bot]")
	}
	for _, assignee := range raw.Assignees {
		pr.Assignees = append(pr.Assignees, graph.User{Login: gitlabLogin(assignee), AvatarURL: assignee.AvatarURL})
	}
	// Keep the historical fields populated for consumers that still decode
	// them, but never infer approval-rule satisfaction from approved_by.
	pr.ReviewApproved = approverCount
	pr.ReviewTotal = approvalRequired
	pr.Relation = graph.RelationFor(pr, viewer, hasReviewer(raw, viewer))
	return pr
}

func gitlabLogin(user rawUser) string {
	if user.Username != "" {
		return user.Username
	}
	return user.Name
}

func hasReviewer(mr rawMergeRequest, username string) bool {
	for _, reviewer := range mr.Reviewers {
		if gitlabLogin(reviewer) == username {
			return true
		}
	}
	return false
}

func pipelineState(mr rawMergeRequest) string {
	pipeline := mr.HeadPipeline
	if pipeline == nil {
		pipeline = mr.Pipeline
	}
	if pipeline == nil || pipeline.Status == "" {
		return "UNKNOWN"
	}
	switch strings.ToLower(pipeline.Status) {
	case "success":
		return "SUCCESS"
	case "failed", "canceled", "cancelled":
		return "FAILURE"
	case "skipped":
		return "SKIPPED"
	case "pending", "running", "created", "manual", "scheduled", "preparing":
		return "PENDING"
	default:
		return strings.ToUpper(pipeline.Status)
	}
}

func mergeState(mr rawMergeRequest) string {
	if mr.HasConflicts || strings.EqualFold(mr.DetailedMergeStatus, "conflict") {
		return "CONFLICTING"
	}
	status := strings.ToLower(mr.DetailedMergeStatus)
	if status == "" {
		status = strings.ToLower(mr.MergeStatus)
	}
	switch status {
	case "mergeable", "can_be_merged", "not_approved", "approvals_syncing":
		return "MERGEABLE"
	default:
		return strings.ToUpper(mr.DetailedMergeStatus)
	}
}

func approvalFields(approval *rawApproval) (state string, required, remaining, approverCount int) {
	if approval == nil {
		return "UNAVAILABLE", 0, 0, 0
	}
	required = approval.ApprovalsRequired
	remaining = approval.ApprovalsLeft
	approverCount = len(approval.ApprovedBy)
	if required <= 0 {
		return "NOT_REQUIRED", required, remaining, approverCount
	}
	if approval.Approved || remaining <= 0 {
		return "APPROVED", required, remaining, approverCount
	}
	return "PENDING", required, remaining, approverCount
}

func approvalDecision(approval *rawApproval) string {
	state, _, _, _ := approvalFields(approval)
	if state == "APPROVED" {
		return "APPROVED"
	}
	return ""
}

// isUpstreamParent enforces the source project/branch side of the GitLab
// project/branch identity. The parent's target project may be another
// project, as in a valid cross-project fork MR.
func isUpstreamParent(parent, child *graph.PullRequest) bool {
	return parent.HeadRepositoryID != "" && parent.HeadRepositoryID == child.RepositoryID && parent.HeadRefName == child.BaseRefName
}

// isDownstreamChild is the corresponding exact identity check for a child MR.
func isDownstreamChild(parent, child *graph.PullRequest) bool {
	return child.RepositoryID == parent.HeadRepositoryID && child.BaseRefName == parent.HeadRefName && child.RepositoryID != "" && child.HeadRepositoryID != ""
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (c *Client) startSpan(ctx context.Context, name string, kind oteltrace.SpanKind, attributes oteltrace.Attributes) (context.Context, oteltrace.Span) {
	if c.Tracer == nil {
		return ctx, nil
	}
	return c.Tracer.Start(ctx, name, kind, attributes)
}
