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
	BLUE    = "\x1b[34m"
)

func sectionHeader(label string) string {
	return BOLD + YELLOW + "── " + label + " ──" + RESET
}

func itemLine(repoShort string, number int, title string, color string, itemType string) string {
	var badge string
	switch itemType {
	case "pr":
		badge = BLUE + "PR" + RESET + "  "
	case "issue":
		badge = GREEN + "IS" + RESET + "  "
	}
	return fmt.Sprintf("  %s%s%s  %s#%d%s  %s%s", color, repoShort, RESET, CYAN, number, RESET, badge, title)
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

	for _, node := range resp.AwaitingApproval.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsAwaitingApprovalSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		logMsg(fmt.Sprintf("awaiting: #%d %s", pr.Number, pr.Title))
		short := repoShortName(pr.Repository.NameWithOwner)
		awaiting = append(awaiting, DashboardItem{Display: itemLine(short, pr.Number, pr.Title, MAGENTA, "pr"), URL: pr.Url})
	}

	for _, node := range resp.ChangesRequested.Nodes {
		pr, ok := node.(*gql.FetchPRSectionsChangesRequestedSearchResultItemConnectionNodesPullRequest)
		if !ok || pr.Number == 0 {
			continue
		}
		short := repoShortName(pr.Repository.NameWithOwner)
		changesRequested = append(changesRequested, DashboardItem{Display: itemLine(short, pr.Number, pr.Title, YELLOW, "pr"), URL: pr.Url})
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
	itemType  string // "pr" or "issue"
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
	dashItem := DashboardItem{Display: itemLine(repoShortName(repo), d.number, d.title, GREEN, d.itemType), URL: d.url}

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

func appendSection(lines []string, header string, items []DashboardItem) []string {
	lines = append(lines, sectionHeader(header)+"\t")
	if len(items) == 0 {
		lines = append(lines, "  (none)\t")
	} else {
		for _, item := range items {
			lines = append(lines, item.Display+"\t"+item.URL)
		}
	}
	return lines
}

func buildLines(awaiting, changesRequested, ready, inProgress []DashboardItem) []string {
	sections := []struct {
		header string
		items  []DashboardItem
	}{
		{"Awaiting Approval", awaiting},
		{"Changes Requested", changesRequested},
		{"Ready", ready},
		{"In Progress", inProgress},
	}
	var lines []string
	for i, s := range sections {
		lines = appendSection(lines, s.header, s.items)
		if i < len(sections)-1 {
			lines = append(lines, "\t")
		}
	}
	return lines
}

// ─── fzf launcher ─────────────────────────────────────────────────────────────

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

	// buildShellPreview builds an fzf preview command string safe for use inside change-preview(…).
	// ifBranches is a partial "if … ; elif … ;" chain (no trailing else/fi) — no ")" in pattern
	// positions — so fzf's paren-depth parser never closes the change-preview( prematurely.
	// leftHint / rightHint are dim navigation labels shown at the bottom of the preview panel.
	//
	// Notes on the shell template:
	//   • GLAMOUR_STYLE is hardcoded to prevent fzf from treating ${GLAMOUR_STYLE:-dark} braces
	//     as a template placeholder and wiping the value.
	//   • GH_FORCE_TTY=1 makes gh render markdown via glamour even when stdout is a pipe.
	//   • ${#L} / ${#R} are POSIX string-length expansions, NOT fzf placeholders.
	buildShellPreview := func(ifBranches, leftHint, rightHint string) string {
		s := `url={2}; ` + ifBranches + ` else echo 'Select an item to preview'; fi`
		if leftHint != "" || rightHint != "" {
			s += `; if printf '%s' "$url" | grep -q '^http'; then ` +
				`L=` + fmt.Sprintf("%q", leftHint) + `; R=` + fmt.Sprintf("%q", rightHint) + `; ` +
				`printf '\n\033[2m%s%*s%s\033[0m' "$L" "$((FZF_PREVIEW_COLUMNS - ${#L} - ${#R}))" "" "$R"` +
				`; fi`
		}
		return s
	}

	ghEnv := `CLICOLOR_FORCE=1 GLAMOUR_STYLE=dark GH_FORCE_TTY=1`

	detailsPreview := buildShellPreview(
		`if printf '%s' "$url" | grep -q '/pull/'; then `+ghEnv+` gh pr view "$url"; `+
			`elif printf '%s' "$url" | grep -q '/issues/'; then `+ghEnv+` gh issue view "$url";`,
		"← Repository", "Comments →",
	)
	commentsPreview := buildShellPreview(
		`if printf '%s' "$url" | grep -q '/pull/'; then `+ghEnv+` gh pr view --comments "$url"; `+
			`elif printf '%s' "$url" | grep -q '/issues/'; then `+ghEnv+` gh issue view --comments "$url";`,
		"← Repository", "",
	)
	repoPreview := buildShellPreview(
		`if printf '%s' "$url" | grep -q '^http'; then `+
			`repo=$(printf '%s' "$url" | sed 's|https://github.com/||; s|/pull/.*||; s|/issues/.*||'); `+
			ghEnv+` gh repo view "$repo";`,
		"", "Details →",
	)

	cmd := exec.Command(
		"fzf",
		"--ansi",
		"--layout=reverse",
		"--border",
		"--delimiter=\t",
		"--with-nth=1",
		"--no-sort",
		"--preview", detailsPreview,
		"--preview-label", " Details ",
		"--preview-window", "right:55%:wrap",
		"--bind", "right:change-preview-label( Comments )+change-preview("+commentsPreview+")+preview-top",
		"--bind", "left:change-preview-label( Repository )+change-preview("+repoPreview+")+preview-top",
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

	orgSuffix := ""
	if *org != "" {
		orgSuffix = " (org: " + *org + ")"
	}
	fmt.Fprintf(os.Stderr, "[Info] Fetching dashboard for: %s%s\n", login, orgSuffix)
	logMsg(fmt.Sprintf("Authenticated as: %s%s", login, orgSuffix))

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
