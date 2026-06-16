package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/browser"
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
)

func sectionHeader(label string) string {
	return BOLD + YELLOW + "── " + label + " ──" + RESET
}

func itemLine(repoShort string, number int, title string, color string) string {
	return fmt.Sprintf("  %s%s%s  %s#%d%s  %s", color, repoShort, RESET, CYAN, number, RESET, title)
}

func repoShortName(nameWithOwner string) string {
	parts := strings.SplitN(nameWithOwner, "/", 2)
	if len(parts) == 2 {
		return parts[1]
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
	fmt.Fprintf(logFile, "[%s] %s\n", ts, msg)
}

// ─── DashboardItem ───────────────────────────────────────────────────────────

type DashboardItem struct {
	Display string
	URL     string
}

// ─── Data fetching ────────────────────────────────────────────────────────────

func fetchPRSections(gqlClient graphql.Client, login, org string) (awaiting []DashboardItem, changesRequested []DashboardItem) {
	search1 := "is:pr is:open review-requested:" + login
	search2 := "is:pr is:open review:changes_requested author:" + login
	if org != "" {
		search1 += " org:" + org
		search2 += " org:" + org
	}

	resp, err := gql.FetchPRSections(context.Background(), gqlClient, search1, search2)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Failed to fetch PR sections: %v\n", err)
		logMsg(fmt.Sprintf("fetchPRSections error: %v", err))
		return
	}

	if logFile != nil {
		for _, node := range resp.AwaitingApproval.Nodes {
			pr, ok := node.(*gql.FetchPRSectionsAwaitingApprovalSearchResultItemConnectionNodesPullRequest)
			if ok {
				logMsg(fmt.Sprintf("awaiting: #%d %s", pr.Number, pr.Title))
			}
		}
	}

	for _, node := range resp.AwaitingApproval.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsAwaitingApprovalSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		short := repoShortName(pr.Repository.NameWithOwner)
		awaiting = append(awaiting, DashboardItem{Display: itemLine(short, pr.Number, pr.Title, MAGENTA), URL: pr.Url})
	}

	for _, node := range resp.ChangesRequested.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsChangesRequestedSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		short := repoShortName(pr.Repository.NameWithOwner)
		changesRequested = append(changesRequested, DashboardItem{Display: itemLine(short, pr.Number, pr.Title, YELLOW), URL: pr.Url})
	}

	return awaiting, changesRequested
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
}

// classifyProjectItem filters and appends a project item to ready or inProgress.
func classifyProjectItem(d projectItemData, login string, ready, inProgress *[]DashboardItem) {
	logMsg(fmt.Sprintf("  item content: number=%d title=%q url=%s", d.number, d.title, d.url))

	if d.number == 0 || d.title == "" || d.url == "" {
		logMsg("  → skip: missing number/title/url")
		return
	}
	if !d.stateOpen {
		logMsg("  → skip: not open")
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
	dashItem := DashboardItem{Display: itemLine(repoShortName(repo), d.number, d.title, GREEN), URL: d.url}

	statusLower := strings.ToLower(d.status)
	switch {
	case strings.Contains(statusLower, "ready"):
		logMsg(fmt.Sprintf("  → Ready: #%d", d.number))
		*ready = append(*ready, dashItem)
	case strings.Contains(statusLower, "in progress"):
		logMsg(fmt.Sprintf("  → In Progress: #%d", d.number))
		*inProgress = append(*inProgress, dashItem)
	default:
		logMsg(fmt.Sprintf("  → skip: status=%q (not ready/in progress)", d.status))
	}
}

func processOrgProjectItem(
	item gql.FetchOrgProjectItemsOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2Item,
	login string,
	ready, inProgress *[]DashboardItem,
) {
	content := item.Content
	if content == nil {
		logMsg("  → skip: no content")
		return
	}

	var d projectItemData
	switch c := content.(type) {
	case *gql.FetchOrgProjectItemsOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemContentIssue:
		d = projectItemData{number: c.Number, title: c.Title, url: c.Url, stateOpen: c.IssueState == gql.IssueStateOpen, repo: c.Repository.NameWithOwner}
		for _, a := range c.Assignees.Nodes {
			d.logins = append(d.logins, a.Login)
		}
	case *gql.FetchOrgProjectItemsOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemContentPullRequest:
		d = projectItemData{number: c.Number, title: c.Title, url: c.Url, stateOpen: c.PrState == gql.PullRequestStateOpen, repo: c.Repository.NameWithOwner}
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

	classifyProjectItem(d, login, ready, inProgress)
}

func processViewerOrgProjectItem(
	item gql.FetchViewerOrgProjectItemsViewerUserOrganizationsOrganizationConnectionNodesOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2Item,
	login string,
	ready, inProgress *[]DashboardItem,
) {
	content := item.Content
	if content == nil {
		logMsg("  → skip: no content")
		return
	}

	var d projectItemData
	switch c := content.(type) {
	case *gql.FetchViewerOrgProjectItemsViewerUserOrganizationsOrganizationConnectionNodesOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemContentIssue:
		d = projectItemData{number: c.Number, title: c.Title, url: c.Url, stateOpen: c.IssueState == gql.IssueStateOpen, repo: c.Repository.NameWithOwner}
		for _, a := range c.Assignees.Nodes {
			d.logins = append(d.logins, a.Login)
		}
	case *gql.FetchViewerOrgProjectItemsViewerUserOrganizationsOrganizationConnectionNodesOrganizationProjectsV2ProjectV2ConnectionNodesProjectV2ItemsProjectV2ItemConnectionNodesProjectV2ItemContentPullRequest:
		d = projectItemData{number: c.Number, title: c.Title, url: c.Url, stateOpen: c.PrState == gql.PullRequestStateOpen, repo: c.Repository.NameWithOwner}
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

	classifyProjectItem(d, login, ready, inProgress)
}

func fetchProjectItems(gqlClient graphql.Client, login, org string) (ready []DashboardItem, inProgress []DashboardItem) {
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
			return
		}
		for _, project := range resp.Organization.ProjectsV2.Nodes {
			logMsg(fmt.Sprintf("Project: %q (%d items)", project.Title, len(project.Items.Nodes)))
			for _, item := range project.Items.Nodes {
				processOrgProjectItem(item, login, &ready, &inProgress)
			}
		}
	} else {
		resp, err := gql.FetchViewerOrgProjectItems(context.Background(), gqlClient)
		if handleErr(err) {
			return
		}
		for _, orgNode := range resp.Viewer.Organizations.Nodes {
			for _, project := range orgNode.ProjectsV2.Nodes {
				logMsg(fmt.Sprintf("Project: %q (%d items)", project.Title, len(project.Items.Nodes)))
				for _, item := range project.Items.Nodes {
					processViewerOrgProjectItem(item, login, &ready, &inProgress)
				}
			}
		}
	}

	return ready, inProgress
}

// ─── Build fzf lines ──────────────────────────────────────────────────────────

func appendSection(lines *[]string, header string, items []DashboardItem) {
	*lines = append(*lines, sectionHeader(header)+"\t")
	if len(items) == 0 {
		*lines = append(*lines, "  (none)\t")
	} else {
		for _, item := range items {
			*lines = append(*lines, item.Display+"\t"+item.URL)
		}
	}
}

func buildLines(awaiting, changesRequested, ready, inProgress []DashboardItem) []string {
	var lines []string
	appendSection(&lines, "Awaiting Approval", awaiting)
	lines = append(lines, "\t")
	appendSection(&lines, "Changes Requested", changesRequested)
	lines = append(lines, "\t")
	appendSection(&lines, "Ready", ready)
	lines = append(lines, "\t")
	appendSection(&lines, "In Progress", inProgress)
	return lines
}

// ─── fzf launcher ─────────────────────────────────────────────────────────────

func printPlain(lines []string) {
	for _, l := range lines {
		parts := strings.SplitN(l, "\t", 2)
		fmt.Println(parts[0])
	}
}

func launchFzf(lines []string) {
	_, err := exec.LookPath("fzf")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] fzf not found. Falling back to plain text output.\n")
		printPlain(lines)
		return
	}

	cmd := exec.Command(
		"fzf",
		"--ansi",
		"--layout=reverse",
		"--border",
		"--delimiter=\t",
		"--with-nth=1",
		"--no-sort",
	)
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n"))
	cmd.Stderr = os.Stderr
	// RUNEWIDTH_EASTASIAN=0 prevents fzf from treating box-drawing chars as
	// double-width in East Asian locales, which would halve the border width.
	cmd.Env = append(os.Environ(), "RUNEWIDTH_EASTASIAN=0")

	var outBuf strings.Builder
	cmd.Stdout = &outBuf

	err = cmd.Run()
	selected := strings.TrimSpace(outBuf.String())
	if selected == "" {
		return
	}

	parts := strings.SplitN(selected, "\t", 2)
	if len(parts) < 2 {
		return
	}
	url := strings.TrimSpace(parts[1])
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
	flag.Parse()

	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[Error] Cannot open log file: %v\n", err)
		} else {
			logFile = f
			defer f.Close()
		}
	}

	logMsg(fmt.Sprintf("Starting gh-dashboard (dry-run=%v)", *dryRun))

	httpClient, err := api.NewHTTPClient(api.ClientOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Failed to create HTTP client: %v\n", err)
		os.Exit(1)
	}
	gqlClient := graphql.NewClient("https://api.github.com/graphql", httpClient)

	viewerResp, err := gql.GetViewer(context.Background(), gqlClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Failed to get authenticated user: %v\n", err)
		os.Exit(1)
	}
	login := viewerResp.Viewer.Login

	if *org != "" {
		fmt.Fprintf(os.Stderr, "[Info] Fetching dashboard for: %s (org: %s)\n", login, *org)
		logMsg(fmt.Sprintf("Authenticated as: %s (org: %s)", login, *org))
	} else {
		fmt.Fprintf(os.Stderr, "[Info] Fetching dashboard for: %s\n", login)
		logMsg(fmt.Sprintf("Authenticated as: %s", login))
	}

	var (
		awaiting         []DashboardItem
		changesRequested []DashboardItem
		ready            []DashboardItem
		inProgress       []DashboardItem
		wg               sync.WaitGroup
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		awaiting, changesRequested = fetchPRSections(gqlClient, login, *org)
	}()
	go func() {
		defer wg.Done()
		ready, inProgress = fetchProjectItems(gqlClient, login, *org)
	}()
	wg.Wait()

	logMsg(fmt.Sprintf("Summary: awaiting=%d changesRequested=%d ready=%d inProgress=%d",
		len(awaiting), len(changesRequested), len(ready), len(inProgress)))

	lines := buildLines(awaiting, changesRequested, ready, inProgress)

	if *dryRun {
		printPlain(lines)
		if *logPath != "" {
			fmt.Fprintf(os.Stderr, "[Info] Log written to: %s\n", *logPath)
		}
		return
	}

	launchFzf(lines)
}
