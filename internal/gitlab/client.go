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

// MaxMRs and MaxDepth are intentionally conservative.  A malformed or very
// large stack must not turn a refresh into an unbounded API walk.
const (
	defaultMaxMRs   = 500
	defaultMaxDepth = 20
)

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
	Hostname string
	MaxMRs   int
	MaxDepth int
	Runner   CommandRunner
	Tracer   oteltrace.Tracer

	projectMu    sync.Mutex
	projectCache map[int]*rawProject
}

func New(hostname string) *Client {
	return &Client{
		Hostname:     hostname,
		MaxMRs:       defaultMaxMRs,
		MaxDepth:     defaultMaxDepth,
		Runner:       execRunner{},
		projectCache: make(map[int]*rawProject),
	}
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
	ApprovalsRequired int `json:"approvals_required"`
	ApprovalsLeft     int `json:"approvals_left"`
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

// Load implements server.Loader.
func (c *Client) Load(ctx context.Context, options graph.SearchOptions) (graph.Result, error) {
	return c.LoadProgress(ctx, options, nil)
}

// LoadProgress searches, hydrates and discovers a bounded merge-request graph.
func (c *Client) LoadProgress(ctx context.Context, options graph.SearchOptions, progress func(current, total int, phase string, collected int)) (result graph.Result, resultErr error) {
	ctx, span := c.startSpan(ctx, "load merge request graph", oteltrace.SpanInternal, oteltrace.Attributes{"mr.search_query": options.Query})
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
	viewer, err := c.currentUser(ctx)
	if err != nil {
		return graph.Result{}, err
	}
	specs := buildSearchSpecs(options)
	if len(specs) == 0 {
		return graph.Build(nil, nil), nil
	}
	report(0, len(specs), "Searching merge requests", 0)
	byID := make(map[string]*graph.PullRequest)
	rawByID := make(map[string]rawMergeRequest)
	directReview := make(map[string]bool)
	warnings := make([]string, 0)
	searchLimitReached := false
	for i, spec := range specs {
		rawItems, listErr := c.listMergeRequests(ctx, spec)
		if listErr != nil {
			return graph.Result{}, listErr
		}
		for _, raw := range rawItems {
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
			if spec.scope == "reviews_for_me" {
				directReview[id] = true
			}
			if _, exists := rawByID[id]; !exists {
				rawByID[id] = raw
			}
		}
		report(i+1, len(specs), "Searching merge requests", len(rawByID))
	}

	// Hydrate search results before constructing graph DTOs.  A failed detail
	// or approval request does not discard the MR; it only leaves the fields
	// available from the list response.
	searchItems := make([]rawMergeRequest, 0, len(rawByID))
	for _, raw := range rawByID {
		searchItems = append(searchItems, raw)
	}
	sort.Slice(searchItems, func(i, j int) bool { return rawMRID(searchItems[i]) < rawMRID(searchItems[j]) })
	hydrated, hydrateWarnings := c.hydrateMany(ctx, searchItems)
	warnings = append(warnings, hydrateWarnings...)
	for _, item := range hydrated {
		id := rawMRID(item.mr)
		pr := c.convert(item.mr, item.approval, viewer.Username, "search")
		if pr == nil {
			continue
		}
		pr.Relation = graph.RelationFor(pr, viewer.Username, directReview[id] || hasReviewer(item.mr, viewer.Username))
		byID[id] = pr
	}

	seeds := make([]*graph.PullRequest, 0, len(byID))
	for _, pr := range byID {
		seeds = append(seeds, pr)
	}
	report(0, len(seeds), "Discovering stacked merge requests", len(byID))
	discovered, discoveryWarnings := c.discoverStacks(ctx, seeds, byID, viewer.Username, report)
	warnings = append(warnings, discoveryWarnings...)
	if searchLimitReached {
		warnings = append(warnings, "Merge request limit reached; narrow the search to see the complete graph.")
	}
	if discovered {
		warnings = append(warnings, "Merge request limit reached; narrow the search to see the complete graph.")
	}
	prs := make([]*graph.PullRequest, 0, len(byID))
	for _, pr := range byID {
		prs = append(prs, pr)
	}
	return graph.Build(prs, warnings), nil
}

type hydratedMR struct {
	mr       rawMergeRequest
	approval *rawApproval
}

func (c *Client) hydrateMany(ctx context.Context, items []rawMergeRequest) ([]hydratedMR, []string) {
	if len(items) == 0 {
		return nil, nil
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
	ordered := make([]hydratedMR, len(items))
	warnings := make([]string, 0)
	for item := range responses {
		ordered[item.index] = hydratedMR{mr: item.mr, approval: item.approval}
		if item.detailErr != nil {
			warnings = append(warnings, "Some GitLab merge request details could not be loaded; list data was retained.")
		}
		if item.approvalErr != nil {
			// Approval rules are optional (and unavailable on some tiers).  Keep
			// the MR visible and let the converter fall back to reviewers.
			warnings = append(warnings, "Some GitLab approval data is unavailable; reviewer counts may be incomplete.")
		}
	}
	return ordered, uniqueWarnings(warnings)
}

func (c *Client) hydrateOne(ctx context.Context, mr rawMergeRequest) (rawMergeRequest, *rawApproval, error, error) {
	detail := mr
	var detailErr error
	if mr.TargetProjectID > 0 && mr.IID > 0 {
		var fetched rawMergeRequest
		detailErr = c.apiJSON(ctx, "get merge request detail", mergeRequestPath(mr.TargetProjectID, mr.IID), false, &fetched)
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
		approvalErr = c.apiJSON(ctx, "get merge request approvals", approvalPath(detail.TargetProjectID, detail.IID), false, &approval)
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

func (c *Client) discoverStacks(ctx context.Context, seeds []*graph.PullRequest, byID map[string]*graph.PullRequest, viewer string, report func(int, int, string, int)) (bool, []string) {
	frontier := make([]stackItem, 0, len(seeds))
	for _, seed := range seeds {
		frontier = append(frontier, stackItem{pr: seed, direction: stackBoth})
	}
	visitedDownstream := make(map[string]bool)
	visitedUpstream := make(map[string]bool)
	warnings := make([]string, 0)
	limitReached := false
	processed := 0

	// Direct search seeds can be explored in both directions. A node discovered
	// upstream or downstream retains that direction, so an upstream parent
	// cannot make the traversal fan out into its unrelated downstream siblings.
	for len(frontier) > 0 && len(byID) < c.maxMRs() {
		next := make([]stackItem, 0)
		for _, current := range frontier {
			processed++
			if current.depth >= c.maxDepth() {
				continue
			}
			if (current.direction == stackBoth || current.direction == stackDownstream) && current.pr.HeadRepositoryID != "" && current.pr.HeadRefName != "" {
				key := refKey(current.pr.HeadRepositoryID, current.pr.HeadRefName)
				if !visitedDownstream[key] {
					visitedDownstream[key] = true
					rawItems, err := c.listBranchMergeRequests(ctx, branchSearch{projectID: current.pr.HeadRepositoryID, targetBranch: current.pr.HeadRefName})
					if err != nil {
						warnings = append(warnings, "Could not discover downstream GitLab merge requests for a stack branch.")
					} else {
						added, limit := c.addDiscovered(ctx, rawItems, byID, viewer, "downstream", stackDownstream, current.depth+1, &next, current.pr)
						if added > 0 {
							report(processed, max(1, len(seeds)), "Discovering stacked merge requests", len(byID))
						}
						limitReached = limitReached || limit
					}
				}
			}
			if (current.direction == stackBoth || current.direction == stackUpstream) && current.pr.RepositoryID != "" && current.pr.BaseRefName != "" && current.pr.BaseRefName != current.pr.DefaultBranch {
				key := refKey(current.pr.RepositoryID, current.pr.BaseRefName)
				if !visitedUpstream[key] {
					visitedUpstream[key] = true
					rawItems, err := c.listBranchMergeRequests(ctx, branchSearch{projectID: current.pr.RepositoryID, sourceBranch: current.pr.BaseRefName})
					if err != nil {
						warnings = append(warnings, "Could not discover upstream GitLab merge requests for a stack branch.")
					} else {
						added, limit := c.addDiscovered(ctx, rawItems, byID, viewer, "upstream", stackUpstream, current.depth+1, &next, current.pr)
						if added > 0 {
							report(processed, max(1, len(seeds)), "Discovering stacked merge requests", len(byID))
						}
						limitReached = limitReached || limit
					}
				}
			}
		}
		frontier = next
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

func (c *Client) addDiscovered(ctx context.Context, rawItems []rawMergeRequest, byID map[string]*graph.PullRequest, viewer, source string, direction stackDirection, depth int, next *[]stackItem, current *graph.PullRequest) (int, bool) {
	added, limitReached := 0, false
	for _, raw := range rawItems {
		if len(byID) >= c.maxMRs() {
			limitReached = true
			break
		}
		id := rawMRID(raw)
		if id == "" || id == current.ID {
			continue
		}
		// The API query narrows candidates, but this second check is what
		// protects against GitLab versions that ignore one of the branch
		// filters and against same-named branches in forks.
		if source == "downstream" {
			if strconv.Itoa(raw.TargetProjectID) != current.HeadRepositoryID || raw.TargetBranch != current.HeadRefName {
				continue
			}
		} else {
			if strconv.Itoa(raw.SourceProjectID) != current.RepositoryID || raw.SourceBranch != current.BaseRefName || strconv.Itoa(raw.TargetProjectID) != current.RepositoryID {
				continue
			}
		}
		if _, exists := byID[id]; exists {
			continue
		}
		items, _ := c.hydrateMany(ctx, []rawMergeRequest{raw})
		if len(items) == 0 {
			continue
		}
		pr := c.convert(items[0].mr, items[0].approval, viewer, source)
		if pr == nil {
			continue
		}
		if source == "downstream" {
			if !isDownstreamChild(current, pr) {
				continue
			}
		} else if !isUpstreamParent(pr, current) {
			continue
		}
		pr.Relation = graph.RelationFor(pr, viewer, hasReviewer(items[0].mr, viewer))
		byID[id] = pr
		*next = append(*next, stackItem{pr: pr, depth: depth, direction: direction})
		added++
	}
	return added, limitReached
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

func (c *Client) listMergeRequests(ctx context.Context, spec searchSpec) ([]rawMergeRequest, error) {
	values := url.Values{"per_page": {"100"}, "scope": {spec.scope}, "state": {"opened"}}
	if spec.query != "" {
		values.Set("search", spec.query)
	}
	return c.listAPI(ctx, "list merge requests", queryPath("/merge_requests", values))
}

func (c *Client) listBranchMergeRequests(ctx context.Context, search branchSearch) ([]rawMergeRequest, error) {
	values := url.Values{"per_page": {"100"}, "state": {"opened"}}
	if search.sourceBranch != "" {
		values.Set("source_branch", search.sourceBranch)
	}
	if search.targetBranch != "" {
		values.Set("target_branch", search.targetBranch)
	}
	return c.listAPI(ctx, "list stacked merge requests", queryPath("/projects/"+url.PathEscape(search.projectID)+"/merge_requests", values))
}

func (c *Client) listAPI(ctx context.Context, operation, endpoint string) ([]rawMergeRequest, error) {
	var pages []rawMergeRequest
	if err := c.apiJSON(ctx, operation, endpoint, true, &pages); err != nil {
		return nil, err
	}
	return pages, nil
}

func (c *Client) currentUser(ctx context.Context) (rawUser, error) {
	var user rawUser
	if err := c.apiJSON(ctx, "get current GitLab user", "/user", false, &user); err != nil {
		return rawUser{}, err
	}
	if user.Username == "" {
		return rawUser{}, errors.New("GitLab user response did not contain a username")
	}
	return user, nil
}

func (c *Client) apiJSON(ctx context.Context, operation, endpoint string, paginate bool, target any) error {
	args := []string{"api"}
	if paginate {
		args = append(args, "--paginate")
	}
	if c.Hostname != "" {
		args = append(args, "--hostname", c.Hostname)
	}
	args = append(args, endpoint)
	_, span := c.startSpan(ctx, "glab api: "+operation, oteltrace.SpanClient, oteltrace.Attributes{
		"process.executable.name": "glab",
		"process.command_args":    safeCommandArgs(args),
		"gitlab.operation.name":   operation,
	})
	out, err := c.runner().Run(ctx, args)
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

func safeCommandArgs(args []string) []string {
	result := make([]string, len(args))
	copy(result, args)
	return result
}

func (c *Client) runner() CommandRunner {
	if c.Runner != nil {
		return c.Runner
	}
	return execRunner{}
}

// decodeJSONPages accepts both one JSON value and the newline-separated JSON
// arrays emitted by glab api --paginate.  Keeping it independent of the
// command runner makes pagination fixtures straightforward to write.
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
	c.projectMu.Unlock()
	var project rawProject
	if err := c.apiJSON(ctx, "get GitLab project", "/projects/"+strconv.Itoa(id), false, &project); err != nil {
		return nil, err
	}
	c.projectMu.Lock()
	c.projectCache[id] = &project
	c.projectMu.Unlock()
	return &project, nil
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
	pr := &graph.PullRequest{
		ID: id, Number: raw.IID, Title: raw.Title, URL: raw.WebURL, IsDraft: raw.Draft || raw.WorkInProgress,
		UpdatedAt: raw.UpdatedAt, RepositoryID: strconv.Itoa(raw.TargetProjectID), Repository: target.PathWithNamespace, RepositoryURL: target.WebURL, Provider: "gitlab",
		DefaultBranch: target.DefaultBranch, BaseRefName: raw.TargetBranch, HeadRefName: raw.SourceBranch,
		HeadRepositoryID: strconv.Itoa(raw.SourceProjectID), HeadRepository: sourceProject.PathWithNamespace,
		Assignees: make([]graph.User, 0, len(raw.Assignees)), Relation: "other", Source: source,
		CIState: pipelineState(raw), Mergeable: mergeState(raw), ReviewDecision: approvalDecision(approval),
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
	if approval != nil {
		pr.ReviewApproved = len(approval.ApprovedBy)
		pr.ReviewTotal = approval.ApprovalsRequired
		if pr.ReviewTotal == 0 && len(raw.Reviewers) > 0 {
			pr.ReviewTotal = len(raw.Reviewers)
		}
		if pr.ReviewApproved == 0 && approval.ApprovalsRequired > 0 && approval.ApprovalsLeft <= approval.ApprovalsRequired {
			pr.ReviewApproved = approval.ApprovalsRequired - approval.ApprovalsLeft
		}
	} else {
		pr.ReviewTotal = len(raw.Reviewers)
	}
	if pr.ReviewTotal < pr.ReviewApproved {
		pr.ReviewTotal = pr.ReviewApproved
	}
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

func approvalDecision(approval *rawApproval) string {
	if approval == nil || approval.ApprovalsRequired <= 0 {
		return ""
	}
	if approval.ApprovalsLeft <= 0 {
		return "APPROVED"
	}
	return ""
}

// isUpstreamParent enforces both sides of the GitLab project/branch identity.
// In particular, an identically named branch in a fork is never accepted.
func isUpstreamParent(parent, child *graph.PullRequest) bool {
	return parent.HeadRepositoryID != "" && parent.HeadRepositoryID == child.RepositoryID && parent.HeadRefName == child.BaseRefName && parent.RepositoryID == child.RepositoryID
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

func (c *Client) startSpan(ctx context.Context, name string, kind oteltrace.SpanKind, attributes oteltrace.Attributes) (context.Context, oteltrace.Span) {
	if c.Tracer == nil {
		return ctx, nil
	}
	return c.Tracer.Start(ctx, name, kind, attributes)
}
