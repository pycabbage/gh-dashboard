package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/browser"
	"github.com/cli/go-gh/v2/pkg/repository"
	gql "github.com/pycabbage/gh-dashboard/gql"
)

// ─── ANSI helpers ────────────────────────────────────────────────────────────

const (
	RESET   = "\x1b[0m"
	BOLD    = "\x1b[1m"
	YELLOW  = "\x1b[33m"
	CYAN    = "\x1b[36m"
	GREEN   = "\x1b[32m"
	MAGENTA = "\x1b[35m"
	BLUE    = "\x1b[34m"
	RED     = "\x1b[31m"
)

func sectionHeader(label string) string {
	return BOLD + YELLOW + "── " + label + " ──" + RESET
}

func repoShortName(nameWithOwner string) string {
	_, after, ok := strings.Cut(nameWithOwner, "/")
	if ok {
		return after
	}
	return nameWithOwner
}

// ─── Logging ─────────────────────────────────────────────────────────────────

var logFile io.Writer

func logMsg(msg string) {
	if logFile == nil {
		return
	}
	ts := time.Now().Format(time.RFC3339)
	_, _ = fmt.Fprintf(logFile, "[%s] %s\n", ts, msg)
}

// ─── DashboardItem ───────────────────────────────────────────────────────────

// DashboardItem holds raw display data. Formatting is deferred to buildLines
// so all columns can be aligned in a single pass over all sections.
type DashboardItem struct {
	Repo   string // short repo name (no owner prefix)
	Number int
	Badge  string // "PR" or "Issue"
	Title  string
	Color  string // ANSI color applied to the repo column
	URL    string
}

func newItem(repo string, number int, badge, title, color, url string) DashboardItem {
	return DashboardItem{
		Repo:   repoShortName(repo),
		Number: number,
		Badge:  badge,
		Title:  title,
		Color:  color,
		URL:    url,
	}
}

// ─── Data fetching ────────────────────────────────────────────────────────────

func fetchPRSections(gqlClient graphql.Client, searchLogin, org, repo string) (awaiting, changesRequested, reviewed, draftPRs, noReviewPRs []DashboardItem, viewerLogin string) {
	search1 := "is:pr is:open review-requested:" + searchLogin
	search2 := "is:pr is:open review:changes_requested author:" + searchLogin
	search3 := "is:pr is:open reviewed-by:" + searchLogin + " -author:" + searchLogin
	search4 := "is:pr is:open is:draft assignee:" + searchLogin
	search5 := "is:pr is:open author:" + searchLogin + " -is:draft"
	if org != "" {
		search1 += " org:" + org
		search2 += " org:" + org
		search3 += " org:" + org
		search4 += " org:" + org
		search5 += " org:" + org
	}
	if repo != "" {
		search1 += " repo:" + repo
		search2 += " repo:" + repo
		search3 += " repo:" + repo
		search4 += " repo:" + repo
		search5 += " repo:" + repo
	}

	resp, err := gql.FetchPRSections(context.Background(), gqlClient, search1, search2, search3, search4, search5)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Failed to fetch PR sections: %v\n", err)
		logMsg(fmt.Sprintf("fetchPRSections error: %v", err))
		return
	}

	viewerLogin = resp.Viewer.Login

	for _, node := range resp.AwaitingApproval.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsAwaitingApprovalSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		logMsg(fmt.Sprintf("awaiting: #%d %s", pr.Number, pr.Title))
		awaiting = append(awaiting, newItem(pr.Repository.NameWithOwner, pr.Number, "PR", pr.Title, MAGENTA, pr.Url))
	}

	for _, node := range resp.ChangesRequested.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsChangesRequestedSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		changesRequested = append(changesRequested, newItem(pr.Repository.NameWithOwner, pr.Number, "PR", pr.Title, YELLOW, pr.Url))
	}

	for _, node := range resp.Reviewed.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsReviewedSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		reviewed = append(reviewed, newItem(pr.Repository.NameWithOwner, pr.Number, "PR", pr.Title, CYAN, pr.Url))
	}

	for _, node := range resp.MyDraftPRs.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsMyDraftPRsSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		draftPRs = append(draftPRs, newItem(pr.Repository.NameWithOwner, pr.Number, "PR", pr.Title, BLUE, pr.Url))
	}

	for _, node := range resp.MyOpenPRs.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsMyOpenPRsSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		if pr.ReviewRequests.TotalCount == 0 {
			logMsg(fmt.Sprintf("noReviewPRs: #%d %s", pr.Number, pr.Title))
			noReviewPRs = append(noReviewPRs, newItem(pr.Repository.NameWithOwner, pr.Number, "PR", pr.Title, RED, pr.Url))
		}
	}

	return
}

// projectItemData holds the common fields extracted from either query's item type.
type projectItemData struct {
	number    int
	title     string
	url       string
	stateOpen bool
	repo      string
	logins    []string
	status    string
	itemType  string // "pr" or "issue"
}

// classifyProjectItem filters and appends a project item to ready, inReview, or inProgress.
func classifyProjectItem(d projectItemData, login, repoFilter string, ready, inReview, inProgress *[]DashboardItem) {
	logMsg(fmt.Sprintf("  item content: number=%d title=%q url=%s", d.number, d.title, d.url))

	if d.number == 0 || d.title == "" || d.url == "" {
		logMsg("  → skip: missing number/title/url")
		return
	}
	if !d.stateOpen {
		logMsg("  → skip: not open")
		return
	}
	if repoFilter != "" && !strings.EqualFold(d.repo, repoFilter) {
		logMsg(fmt.Sprintf("  → skip: repo %q doesn't match filter %q", d.repo, repoFilter))
		return
	}

	isAssigned := false
	for _, l := range d.logins {
		if l == login {
			isAssigned = true
			break
		}
	}
	logMsg(fmt.Sprintf("  assignees: %v isAssigned=%v", d.logins, isAssigned))
	if !isAssigned {
		logMsg("  → skip: not assigned")
		return
	}

	logMsg(fmt.Sprintf("  status: %s", d.status))
	if d.status == "" {
		logMsg("  → skip: no status field")
		return
	}

	repo := d.repo
	if repo == "" {
		repo = "unknown/unknown"
	}

	badge := "PR"
	if d.itemType == "issue" {
		badge = "Issue"
	}
	item := newItem(repo, d.number, badge, d.title, GREEN, d.url)

	statusLower := strings.ToLower(d.status)
	switch {
	case strings.Contains(statusLower, "ready"):
		logMsg(fmt.Sprintf("  → Ready: #%d", d.number))
		*ready = append(*ready, item)
	case strings.Contains(statusLower, "in review"):
		logMsg(fmt.Sprintf("  → In Review: #%d", d.number))
		*inReview = append(*inReview, item)
	case strings.Contains(statusLower, "in progress"):
		logMsg(fmt.Sprintf("  → In Progress: #%d", d.number))
		*inProgress = append(*inProgress, item)
	default:
		logMsg(fmt.Sprintf("  → skip: status=%q (not ready/in review/in progress)", d.status))
	}
}

func processOrgProjectItem(
	item gql.FetchOrgProjectItemsOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2Item,
	login, repoFilter string,
	ready, inReview, inProgress *[]DashboardItem,
) {
	content := item.Content
	if content == nil {
		logMsg("  → skip: no content")
		return
	}

	var d projectItemData
	switch c := content.(type) {
	case *gql.FetchOrgProjectItemsOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemContentIssue:
		d = projectItemData{number: c.Number, title: c.Title, url: c.Url, stateOpen: c.IssueState == gql.IssueStateOpen, repo: c.Repository.NameWithOwner, itemType: "issue"}
		for _, a := range c.Assignees.Nodes {
			d.logins = append(d.logins, a.Login)
		}
	case *gql.FetchOrgProjectItemsOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemContentPullRequest:
		d = projectItemData{number: c.Number, title: c.Title, url: c.Url, stateOpen: c.PrState == gql.PullRequestStateOpen, repo: c.Repository.NameWithOwner, itemType: "pr"}
		for _, a := range c.Assignees.Nodes {
			d.logins = append(d.logins, a.Login)
		}
	default:
		logMsg("  → skip: not issue or PR")
		return
	}

	for _, fvNode := range item.FieldValues.Nodes {
		ssv, ok := fvNode.(*gql.FetchOrgProjectItemsOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemFieldValuesProjectV2ItemFieldValueConnectionNodesProjectV2ItemFieldSingleSelectValue)
		if !ok || ssv.Name == "" {
			continue
		}
		ssField, ok := ssv.Field.(*gql.FetchOrgProjectItemsOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemFieldValuesProjectV2ItemFieldValueConnectionNodesProjectV2ItemFieldSingleSelectValueFieldProjectV2SingleSelectField)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(ssField.Name), "status") {
			d.status = ssv.Name
			break
		}
	}

	classifyProjectItem(d, login, repoFilter, ready, inReview, inProgress)
}

func processViewerOrgProjectItem(
	item gql.FetchViewerOrgProjectItemsViewerUserOrganizationsOrganizationConnectionNodesOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2Item,
	login, repoFilter string,
	ready, inReview, inProgress *[]DashboardItem,
) {
	content := item.Content
	if content == nil {
		logMsg("  → skip: no content")
		return
	}

	var d projectItemData
	switch c := content.(type) {
	case *gql.FetchViewerOrgProjectItemsViewerUserOrganizationsOrganizationConnectionNodesOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemContentIssue:
		d = projectItemData{number: c.Number, title: c.Title, url: c.Url, stateOpen: c.IssueState == gql.IssueStateOpen, repo: c.Repository.NameWithOwner, itemType: "issue"}
		for _, a := range c.Assignees.Nodes {
			d.logins = append(d.logins, a.Login)
		}
	case *gql.FetchViewerOrgProjectItemsViewerUserOrganizationsOrganizationConnectionNodesOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemContentPullRequest:
		d = projectItemData{number: c.Number, title: c.Title, url: c.Url, stateOpen: c.PrState == gql.PullRequestStateOpen, repo: c.Repository.NameWithOwner, itemType: "pr"}
		for _, a := range c.Assignees.Nodes {
			d.logins = append(d.logins, a.Login)
		}
	default:
		logMsg("  → skip: not issue or PR")
		return
	}

	for _, fvNode := range item.FieldValues.Nodes {
		ssv, ok := fvNode.(*gql.FetchViewerOrgProjectItemsViewerUserOrganizationsOrganizationConnectionNodesOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemFieldValuesProjectV2ItemFieldValueConnectionNodesProjectV2ItemFieldSingleSelectValue)
		if !ok || ssv.Name == "" {
			continue
		}
		ssField, ok := ssv.Field.(*gql.FetchViewerOrgProjectItemsViewerUserOrganizationsOrganizationConnectionNodesOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemFieldValuesProjectV2ItemFieldValueConnectionNodesProjectV2ItemFieldSingleSelectValueFieldProjectV2SingleSelectField)
		if !ok {
			continue
		}
		if strings.Contains(strings.ToLower(ssField.Name), "status") {
			d.status = ssv.Name
			break
		}
	}

	classifyProjectItem(d, login, repoFilter, ready, inReview, inProgress)
}

type rawProjectResponse struct {
	orgResp    *gql.FetchOrgProjectItemsResponse
	viewerResp *gql.FetchViewerOrgProjectItemsResponse
	err        error
}

func fetchProjectItemsRaw(gqlClient graphql.Client, org string) rawProjectResponse {
	handleErr := func(err error) bool {
		if err == nil {
			return false
		}
		msg := err.Error()
		logMsg(fmt.Sprintf("fetchProjectItems error: %s", msg))
		if strings.Contains(msg, "required scopes") || strings.Contains(msg, "read:project") {
			fmt.Fprintf(os.Stderr, "[Error] Fetching project items requires the read:project scope.\n  → Run: gh auth refresh --scopes read:project\n")
		} else {
			fmt.Fprintf(os.Stderr, "[Error] Failed to fetch project items: %v\n", err)
		}
		return true
	}

	if org != "" {
		resp, err := gql.FetchOrgProjectItems(context.Background(), gqlClient, org)
		if handleErr(err) {
			return rawProjectResponse{err: err}
		}
		return rawProjectResponse{orgResp: resp}
	}
	resp, err := gql.FetchViewerOrgProjectItems(context.Background(), gqlClient)
	if handleErr(err) {
		return rawProjectResponse{err: err}
	}
	return rawProjectResponse{viewerResp: resp}
}

func processRawProjectItems(raw rawProjectResponse, login, repoFilter string) (ready, inReview, inProgress []DashboardItem) {
	if raw.err != nil {
		return
	}
	if raw.orgResp != nil {
		for _, project := range raw.orgResp.Organization.ProjectsV2.Nodes {
			logMsg(fmt.Sprintf("Project: %q (%d items)", project.Title, len(project.Items.Nodes)))
			for _, item := range project.Items.Nodes {
				processOrgProjectItem(item, login, repoFilter, &ready, &inReview, &inProgress)
			}
		}
	} else if raw.viewerResp != nil {
		for _, orgNode := range raw.viewerResp.Viewer.Organizations.Nodes {
			for _, project := range orgNode.ProjectsV2.Nodes {
				logMsg(fmt.Sprintf("Project: %q (%d items)", project.Title, len(project.Items.Nodes)))
				for _, item := range project.Items.Nodes {
					processViewerOrgProjectItem(item, login, repoFilter, &ready, &inReview, &inProgress)
				}
			}
		}
	}
	return
}

// ─── Build fzf lines ──────────────────────────────────────────────────────────

type section struct {
	header string
	items  []DashboardItem
}

// formatItem renders a single item with pre-computed column widths.
// Padding is applied to the plain-text values before adding ANSI codes,
// so fmt.Sprintf width specifiers are not confused by escape sequence bytes.
func formatItem(item DashboardItem, repoWidth, numWidth int) string {
	repoPart := fmt.Sprintf("%-*s", repoWidth, item.Repo)
	numStr := fmt.Sprintf("#%d", item.Number)
	numPart := fmt.Sprintf("%*s", numWidth, numStr) // right-align
	badgeColor := BLUE
	if item.Badge == "Issue" {
		badgeColor = GREEN
	}
	badgePart := fmt.Sprintf("%-5s", item.Badge) // "PR" → "PR   ", "Issue" → "Issue"
	return fmt.Sprintf("  %s%s%s  %s%s%s  %s%s%s  %s",
		item.Color, repoPart, RESET,
		CYAN, numPart, RESET,
		badgeColor, badgePart, RESET,
		item.Title)
}

// buildLines formats all sections into tab-separated fzf lines.
// First pass computes max column widths; second pass formats with consistent padding.
func buildLines(sections []section) []string {
	repoWidth, numWidth := 0, 0
	for _, s := range sections {
		for _, item := range s.items {
			if n := len(item.Repo); n > repoWidth {
				repoWidth = n
			}
			if n := len(fmt.Sprintf("#%d", item.Number)); n > numWidth {
				numWidth = n
			}
		}
	}

	var lines []string
	for i, s := range sections {
		lines = append(lines, sectionHeader(s.header)+"\t")
		if len(s.items) == 0 {
			lines = append(lines, "  (none)\t")
		} else {
			for _, item := range s.items {
				lines = append(lines, formatItem(item, repoWidth, numWidth)+"\t"+item.URL)
			}
		}
		if i < len(sections)-1 {
			lines = append(lines, "\t")
		}
	}
	return lines
}

// ─── fzf launcher ─────────────────────────────────────────────────────────────

// findFreePort returns a free localhost TCP port, or 0 on failure.
func findFreePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func printPlain(lines []string) {
	for _, l := range lines {
		display, _, _ := strings.Cut(l, "\t")
		fmt.Println(display)
	}
}

func launchFzf(lines []string) {
	_, err := exec.LookPath("fzf")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] fzf not found. Falling back to plain text output.\n")
		printPlain(lines)
		return
	}

	// State file tracks current preview mode across key presses.
	stateFile, err := os.CreateTemp("", "gh-dashboard-state-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Cannot create state file: %v\n", err)
		printPlain(lines)
		return
	}
	defer func() { _ = os.Remove(stateFile.Name()) }()
	if _, err := stateFile.WriteString("details"); err != nil {
		printPlain(lines)
		return
	}
	if err := stateFile.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Cannot close state file: %v\n", err)
		printPlain(lines)
		return
	}
	sf := stateFile.Name()

	// fzf --listen port lets execute-silent post label-update actions back to fzf.
	port := findFreePort()

	ghEnv := `CLICOLOR_FORCE=1 GLAMOUR_STYLE=dark GH_FORCE_TTY=1`

	// Unified preview command that reads sf and shows the appropriate content.
	// Notes:
	//   • GLAMOUR_STYLE is hardcoded to prevent fzf from substituting ${...} templates.
	//   • ${#L} / ${#R} are POSIX string-length expansions, NOT fzf placeholders.
	previewScript := `url={2}; state=$(cat ` + sf + `); ` +
		`if printf '%s' "$url" | grep -q '/pull/'; then ` +
		`case "$state" in ` +
		`details) ` + ghEnv + ` gh pr view "$url";; ` +
		`comments) ` + ghEnv + ` gh pr view --comments "$url";; ` +
		`*) repo=$(printf '%s' "$url" | sed 's|https://github.com/||; s|/pull/.*||'); ` + ghEnv + ` gh repo view "$repo";; ` +
		`esac; ` +
		`elif printf '%s' "$url" | grep -q '/issues/'; then ` +
		`case "$state" in ` +
		`details) ` + ghEnv + ` gh issue view "$url";; ` +
		`comments) ` + ghEnv + ` gh issue view --comments "$url";; ` +
		`*) repo=$(printf '%s' "$url" | sed 's|https://github.com/||; s|/issues/.*||'); ` + ghEnv + ` gh repo view "$repo";; ` +
		`esac; ` +
		`else echo 'Select an item to preview'; fi; ` +
		`if printf '%s' "$url" | grep -q '^http'; then ` +
		`case "$state" in ` +
		`details) L="← Repository"; R="Comments →";; ` +
		`comments) L="← Details"; R="Repository →";; ` +
		`*) L="← Comments"; R="Details →";; ` +
		`esac; ` +
		`printf '\n\033[2m%s%*s%s\033[0m' "$L" "$((FZF_PREVIEW_COLUMNS - ${#L} - ${#R}))" "" "$R"; ` +
		`fi`

	// buildCycleBind returns an execute-silent action that advances (forward=true) or
	// reverses the 3-state cycle details→comments→repository→details, then POSTs
	// change-preview-label+preview-reload to fzf's --listen server for atomic label update.
	// When no port is available the label is not updated but navigation still works.
	//
	// IMPORTANT: Two fzf parser pitfalls to avoid:
	//
	// 1. execute-silent(CMD) uses balanced-parenthesis matching to find the end of CMD.
	//    Any bare ) inside CMD — e.g. from "case ... in pattern)" — closes the block
	//    prematurely.  We use if/elif/fi instead (no unbalanced parens).
	//
	// 2. Even with if/elif, the string `change-preview-label(%s)` contains a ) that
	//    closes the execute-silent block before the curl pipeline runs, leaving
	//    `+refresh-preview+preview-top' "$nl" | curl ...` to be parsed as fzf action
	//    names → "unknown action: preview-top'...".
	//
	//    Fix: use the colon syntax  execute-silent:CMD  which consumes the rest of the
	//    bind string verbatim (no parenthesis parsing).  Then send refresh-preview and
	//    preview-top as part of the curl POST body so fzf still executes them.
	buildCycleBind := func(forward bool) string {
		var transitions string
		if forward {
			transitions = `if [ "$state" = details ]; then ns=comments; nl=" Comments "; elif [ "$state" = comments ]; then ns=repository; nl=" Repository "; else ns=details; nl=" Details "; fi`
		} else {
			transitions = `if [ "$state" = details ]; then ns=repository; nl=" Repository "; elif [ "$state" = repository ]; then ns=comments; nl=" Comments "; else ns=details; nl=" Details "; fi`
		}
		stateUpdate := `state=$(cat ` + sf + `); ` + transitions + `; printf '%s' "$ns" > ` + sf
		if port > 0 {
			// Colon syntax: execute-silent:CMD — no paren parsing, CMD runs to end of string.
			// refresh-preview and preview-top are sent via the curl POST body so fzf
			// executes them after the label update without needing to chain them here.
			return `execute-silent:` + stateUpdate + `; ` +
				`printf 'change-preview-label(%s)+refresh-preview+preview-top' "$nl" | ` +
				`curl -s -X POST localhost:` + strconv.Itoa(port) + ` -H 'content-type: text/plain' -d @-`
		}
		// No --listen port: state file update only; paren syntax is safe here because
		// stateUpdate (if/elif/fi + printf) contains only balanced parentheses.
		return `execute-silent(` + stateUpdate + `)+refresh-preview+preview-top`
	}

	fzfArgs := []string{
		"--ansi",
		"--layout=reverse",
		"--border",
		"--delimiter=\t",
		"--with-nth=1",
		"--no-sort",
		"--preview", previewScript,
		"--preview-label", " Details ",
		"--preview-window", "right:55%:wrap",
		"--bind", "right:" + buildCycleBind(true),
		"--bind", "left:" + buildCycleBind(false),
	}
	if port > 0 {
		fzfArgs = append(fzfArgs, "--listen", ":"+strconv.Itoa(port))
	}
	cmd := exec.Command("fzf", fzfArgs...)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	cmd.Stderr = os.Stderr
	// RUNEWIDTH_EASTASIAN=0 prevents fzf from treating box-drawing chars as
	// double-width in East Asian locales, which would halve the border width.
	cmd.Env = append(os.Environ(), "RUNEWIDTH_EASTASIAN=0")

	var outBuf strings.Builder
	cmd.Stdout = &outBuf

	_ = cmd.Run()
	selected := strings.TrimSpace(outBuf.String())
	if selected == "" {
		return
	}

	_, urlPart, ok := strings.Cut(selected, "\t")
	if !ok {
		return
	}
	url := strings.TrimSpace(urlPart)
	if !strings.HasPrefix(url, "http") {
		return
	}

	openURL(url)
}

func openURL(url string) {
	b := browser.New("", os.Stdout, os.Stderr)
	if err := b.Browse(url); err != nil {
		fmt.Println("URL:", url)
	}
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	dryRun := flag.Bool("dry-run", false, "print plain text instead of launching fzf")
	logPath := flag.String("log", "", "log file path")
	org := flag.String("org", "", "GitHub organization to scope the dashboard to (optional)")
	actor := flag.String("actor", "", "GitHub username to view the dashboard as")
	var repoFilter string
	flag.StringVar(&repoFilter, "repo", "", "show only items from this repository (owner/repo)")
	flag.StringVar(&repoFilter, "R", "", "show only items from this repository (owner/repo)")
	var showAll bool
	flag.BoolVar(&showAll, "all", false, "show all repositories (default: current repository only)")
	flag.BoolVar(&showAll, "a", false, "show all repositories (default: current repository only)")
	flag.Parse()

	if !showAll {
		if repoFilter != "" {
			if !strings.Contains(repoFilter, "/") {
				fmt.Fprintf(os.Stderr, "[Error] --repo/-R must be in owner/repo format (got %q)\n", repoFilter)
				os.Exit(1)
			}
		} else {
			currentRepo, err := repository.Current()
			if err != nil {
				fmt.Fprintf(os.Stderr, "[Error] Not in a GitHub repository.\n")
				fmt.Fprintf(os.Stderr, "        Use --repo/-R <owner/repo> to specify a repository, or --all/-a to show all.\n")
				os.Exit(1)
			}
			repoFilter = currentRepo.Owner + "/" + currentRepo.Name
		}
	}

	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Error] Cannot open log file: %v\n", err)
		} else {
			logFile = f
			defer func() { _ = f.Close() }()
		}
	}

	logMsg(fmt.Sprintf("Starting gh-dashboard (dry-run=%v)", *dryRun))

	httpClient, err := api.NewHTTPClient(api.ClientOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Failed to create HTTP client: %v\n", err)
		os.Exit(1)
	}
	gqlClient := graphql.NewClient("https://api.github.com/graphql", httpClient)

	// searchLogin is the identity used inside GitHub search strings.
	// When --actor is set, use it directly (known from CLI flag).
	// Otherwise use @me so FetchPRSections can start without a pre-flight GetViewer call.
	searchLogin := "@me"
	if *actor != "" {
		searchLogin = *actor
	}

	var (
		awaiting         []DashboardItem
		changesRequested []DashboardItem
		reviewed         []DashboardItem
		draftPRs         []DashboardItem
		noReviewPRs      []DashboardItem
		ready            []DashboardItem
		inReview         []DashboardItem
		inProgress       []DashboardItem
		viewerLogin      string
		projRaw          rawProjectResponse
		wg               sync.WaitGroup
	)

	fetchProjects := *org != "" || *actor == ""
	wg.Add(1)
	go func() {
		defer wg.Done()
		awaiting, changesRequested, reviewed, draftPRs, noReviewPRs, viewerLogin = fetchPRSections(gqlClient, searchLogin, *org, repoFilter)
	}()
	if fetchProjects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			projRaw = fetchProjectItemsRaw(gqlClient, *org)
		}()
	}
	wg.Wait()

	login := viewerLogin
	if *actor != "" {
		login = *actor
	}
	if login == "" {
		fmt.Fprintf(os.Stderr, "[Error] Failed to determine authenticated user\n")
		os.Exit(1)
	}
	if fetchProjects {
		ready, inReview, inProgress = processRawProjectItems(projRaw, login, repoFilter)
	}

	orgSuffix := ""
	if *org != "" {
		orgSuffix = " (org: " + *org + ")"
	}
	repoSuffix := ""
	if repoFilter != "" {
		repoSuffix = " (repo: " + repoFilter + ")"
	}
	actorSuffix := ""
	if *actor != "" {
		actorSuffix = " (as seen by: " + viewerLogin + ")"
	}
	fmt.Fprintf(os.Stderr, "[Info] Dashboard data for: %s%s%s%s\n", login, orgSuffix, repoSuffix, actorSuffix)
	logMsg(fmt.Sprintf("Actor: %s, Viewer: %s%s%s", login, viewerLogin, orgSuffix, repoSuffix))

	logMsg(fmt.Sprintf("Summary: awaiting=%d changesRequested=%d reviewed=%d noReviewPRs=%d ready=%d inReview=%d inProgress=%d",
		len(awaiting), len(changesRequested), len(reviewed), len(noReviewPRs), len(ready), len(inReview), len(inProgress)))

	sections := []section{
		{"Awaiting Approval", awaiting},
		{"Changes Requested", changesRequested},
		{"No Review Requested", noReviewPRs},
		{"My Draft PRs", draftPRs},
		{"Reviewed by Me", reviewed},
	}
	if fetchProjects {
		sections = append(sections,
			section{"In Review", inReview},
			section{"Ready", ready},
			section{"In Progress", inProgress},
		)
	}
	if *actor != "" && *org == "" {
		fmt.Fprintf(os.Stderr, "[Warning] Project item sections (In Review/Ready/In Progress) are skipped because --org is required when using --actor.\n")
		fmt.Fprintf(os.Stderr, "          Use --org <org-name> to include project items.\n")
	}
	lines := buildLines(sections)

	if *dryRun {
		printPlain(lines)
		if *logPath != "" {
			fmt.Fprintf(os.Stderr, "[Info] Log written to: %s\n", *logPath)
		}
		return
	}

	launchFzf(lines)
}
