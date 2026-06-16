package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
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

// ─── GraphQL types ────────────────────────────────────────────────────────────

type PRNode struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	ReviewDecision string `json:"reviewDecision"`
	Repository     struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

type PRQueryResponse struct {
	AwaitingApproval struct {
		Nodes []PRNode `json:"nodes"`
	} `json:"awaitingApproval"`
	ChangesRequested struct {
		Nodes []PRNode `json:"nodes"`
	} `json:"changesRequested"`
}

type FieldValue struct {
	Name  string `json:"name"`
	Field struct {
		Name string `json:"name"`
	} `json:"field"`
}

type ContentNode struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	State      string `json:"state"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
}

type ProjectItemNode struct {
	FieldValues struct {
		Nodes []FieldValue `json:"nodes"`
	} `json:"fieldValues"`
	Content *ContentNode `json:"content"`
}

type ProjectV2Node struct {
	Title string `json:"title"`
	Items struct {
		Nodes []ProjectItemNode `json:"nodes"`
	} `json:"items"`
}

type ProjectQueryResponse struct {
	Organization struct {
		ProjectsV2 struct {
			Nodes []ProjectV2Node `json:"nodes"`
		} `json:"projectsV2"`
	} `json:"organization"`
}

type ViewerOrgProjectQueryResponse struct {
	Viewer struct {
		Organizations struct {
			Nodes []struct {
				ProjectsV2 struct {
					Nodes []ProjectV2Node `json:"nodes"`
				} `json:"projectsV2"`
			} `json:"nodes"`
		} `json:"organizations"`
	} `json:"viewer"`
}

type DashboardItem struct {
	Display string
	URL     string
}

// ─── Queries ─────────────────────────────────────────────────────────────────

const prQuery = `
query($search1: String!, $search2: String!) {
  awaitingApproval: search(query: $search1, type: ISSUE, first: 50) {
    nodes {
      ... on PullRequest {
        number
        title
        url
        repository { nameWithOwner }
      }
    }
  }
  changesRequested: search(query: $search2, type: ISSUE, first: 50) {
    nodes {
      ... on PullRequest {
        number
        title
        url
        reviewDecision
        repository { nameWithOwner }
      }
    }
  }
}
`

func buildProjectQuery(org string) string {
	return fmt.Sprintf(`
query {
  organization(login: %q) {
    projectsV2(first: 20) {`, org) + `
      nodes {
        title
        items(first: 100) {
          nodes {
            fieldValues(first: 10) {
              nodes {
                ... on ProjectV2ItemFieldSingleSelectValue {
                  name
                  field { ... on ProjectV2SingleSelectField { name } }
                }
              }
            }
            content {
              ... on Issue {
                number title url state
                repository { nameWithOwner }
                assignees(first: 5) { nodes { login } }
              }
              ... on PullRequest {
                number title url state
                repository { nameWithOwner }
                assignees(first: 5) { nodes { login } }
              }
            }
          }
        }
      }
    }
  }
}`
}

const viewerOrgProjectQuery = `
query {
  viewer {
    organizations(first: 20) {
      nodes {
        projectsV2(first: 10) {
          nodes {
            title
            items(first: 50) {
              nodes {
                fieldValues(first: 5) {
                  nodes {
                    ... on ProjectV2ItemFieldSingleSelectValue {
                      name
                      field { ... on ProjectV2SingleSelectField { name } }
                    }
                  }
                }
                content {
                  ... on Issue {
                    number title url state
                    repository { nameWithOwner }
                    assignees(first: 5) { nodes { login } }
                  }
                  ... on PullRequest {
                    number title url state
                    repository { nameWithOwner }
                    assignees(first: 5) { nodes { login } }
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}
`

// ─── Data fetching ────────────────────────────────────────────────────────────

func fetchPRSections(client *api.GraphQLClient, login, org string) (awaiting []PRNode, changesRequested []PRNode) {
	search1 := fmt.Sprintf("is:pr is:open review-requested:%s", login)
	search2 := fmt.Sprintf("is:pr is:open author:%s", login)
	if org != "" {
		search1 += " org:" + org
		search2 += " org:" + org
	}

	vars := map[string]any{
		"search1": search1,
		"search2": search2,
	}

	var resp PRQueryResponse
	err := client.Do(prQuery, vars, &resp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Failed to fetch PR sections: %v\n", err)
		logMsg(fmt.Sprintf("fetchPRSections error: %v", err))
		return []PRNode{}, []PRNode{}
	}

	respJSON, _ := json.MarshalIndent(resp, "", "  ")
	logMsg(fmt.Sprintf("PR query response:\n%s", string(respJSON)))

	for _, n := range resp.AwaitingApproval.Nodes {
		if n.Number != 0 {
			awaiting = append(awaiting, n)
		}
	}

	for _, n := range resp.ChangesRequested.Nodes {
		if n.Number != 0 && n.ReviewDecision == "CHANGES_REQUESTED" {
			changesRequested = append(changesRequested, n)
		}
	}

	if awaiting == nil {
		awaiting = []PRNode{}
	}
	if changesRequested == nil {
		changesRequested = []PRNode{}
	}

	return awaiting, changesRequested
}

func processProjectNodes(projects []ProjectV2Node, login string, ready, inProgress *[]DashboardItem) {
	for _, project := range projects {
		logMsg(fmt.Sprintf("Project: %q (%d items)", project.Title, len(project.Items.Nodes)))

		for _, item := range project.Items.Nodes {
			content := item.Content

			contentJSON, _ := json.Marshal(content)
			fvJSON, _ := json.Marshal(item.FieldValues)
			logMsg(fmt.Sprintf("  item content: %s", string(contentJSON)))
			logMsg(fmt.Sprintf("  item fieldValues: %s", string(fvJSON)))

			if content == nil {
				logMsg("  → skip: no content")
				continue
			}
			if content.Number == 0 || content.Title == "" || content.URL == "" {
				logMsg("  → skip: missing number/title/url")
				continue
			}
			if content.State != "OPEN" {
				logMsg(fmt.Sprintf("  → skip: state=%s", content.State))
				continue
			}

			assignees := content.Assignees.Nodes
			isAssigned := false
			for _, a := range assignees {
				if a.Login == login {
					isAssigned = true
					break
				}
			}
			assigneesJSON, _ := json.Marshal(assignees)
			logMsg(fmt.Sprintf("  assignees: %s isAssigned=%v", string(assigneesJSON), isAssigned))
			if !isAssigned {
				logMsg("  → skip: not assigned")
				continue
			}

			var status string
			for _, fv := range item.FieldValues.Nodes {
				if strings.Contains(strings.ToLower(fv.Field.Name), "status") && fv.Name != "" {
					status = fv.Name
					break
				}
			}
			logMsg(fmt.Sprintf("  status: %s", status))
			if status == "" {
				logMsg("  → skip: no status field")
				continue
			}

			repo := content.Repository.NameWithOwner
			if repo == "" {
				repo = "unknown/unknown"
			}
			short := repoShortName(repo)
			display := itemLine(short, content.Number, content.Title, GREEN)
			dashItem := DashboardItem{Display: display, URL: content.URL}

			statusLower := strings.ToLower(status)
			if strings.Contains(statusLower, "ready") {
				logMsg(fmt.Sprintf("  → Ready: #%d", content.Number))
				*ready = append(*ready, dashItem)
			} else if strings.Contains(statusLower, "in progress") {
				logMsg(fmt.Sprintf("  → In Progress: #%d", content.Number))
				*inProgress = append(*inProgress, dashItem)
			} else {
				logMsg(fmt.Sprintf("  → skip: status=%q (not ready/in progress)", status))
			}
		}
	}
}

func fetchProjectItems(client *api.GraphQLClient, login, org string) (ready []DashboardItem, inProgress []DashboardItem) {
	ready = []DashboardItem{}
	inProgress = []DashboardItem{}

	handleErr := func(err error) bool {
		if err == nil {
			return false
		}
		msg := err.Error()
		logMsg(fmt.Sprintf("fetchProjectItems error: %s", msg))
		if strings.Contains(msg, "required scopes") || strings.Contains(msg, "read:project") {
			fmt.Fprintf(os.Stderr, "[Error] Project items の取得に read:project スコープが必要です。\n  → 次のコマンドを実行してください: gh auth refresh --scopes read:project\n")
		} else {
			fmt.Fprintf(os.Stderr, "[Error] Failed to fetch project items: %v\n", err)
		}
		return true
	}

	if org != "" {
		var resp ProjectQueryResponse
		if err := client.Do(buildProjectQuery(org), nil, &resp); handleErr(err) {
			return
		}
		respJSON, _ := json.MarshalIndent(resp, "", "  ")
		logMsg(fmt.Sprintf("Project query raw response:\n%s", string(respJSON)))
		processProjectNodes(resp.Organization.ProjectsV2.Nodes, login, &ready, &inProgress)
	} else {
		var resp ViewerOrgProjectQueryResponse
		if err := client.Do(viewerOrgProjectQuery, nil, &resp); handleErr(err) {
			return
		}
		respJSON, _ := json.MarshalIndent(resp, "", "  ")
		logMsg(fmt.Sprintf("Viewer org project query raw response:\n%s", string(respJSON)))
		for _, orgNode := range resp.Viewer.Organizations.Nodes {
			processProjectNodes(orgNode.ProjectsV2.Nodes, login, &ready, &inProgress)
		}
	}

	return ready, inProgress
}

// ─── Build fzf lines ──────────────────────────────────────────────────────────

func buildLines(awaiting, changesRequested []PRNode, ready, inProgress []DashboardItem) []string {
	var lines []string

	lines = append(lines, sectionHeader("Awaiting Approval")+"\t")
	if len(awaiting) == 0 {
		lines = append(lines, "  (none)\t")
	} else {
		for _, pr := range awaiting {
			short := repoShortName(pr.Repository.NameWithOwner)
			display := itemLine(short, pr.Number, pr.Title, MAGENTA)
			lines = append(lines, display+"\t"+pr.URL)
		}
	}

	lines = append(lines, "\t")

	lines = append(lines, sectionHeader("Changes Requested")+"\t")
	if len(changesRequested) == 0 {
		lines = append(lines, "  (none)\t")
	} else {
		for _, pr := range changesRequested {
			short := repoShortName(pr.Repository.NameWithOwner)
			display := itemLine(short, pr.Number, pr.Title, YELLOW)
			lines = append(lines, display+"\t"+pr.URL)
		}
	}

	lines = append(lines, "\t")

	lines = append(lines, sectionHeader("Ready")+"\t")
	if len(ready) == 0 {
		lines = append(lines, "  (none)\t")
	} else {
		for _, item := range ready {
			lines = append(lines, item.Display+"\t"+item.URL)
		}
	}

	lines = append(lines, "\t")

	lines = append(lines, sectionHeader("In Progress")+"\t")
	if len(inProgress) == 0 {
		lines = append(lines, "  (none)\t")
	} else {
		for _, item := range inProgress {
			lines = append(lines, item.Display+"\t"+item.URL)
		}
	}

	return lines
}

// ─── fzf launcher ─────────────────────────────────────────────────────────────

func launchFzf(lines []string) {
	input := strings.Join(lines, "\n")

	_, err := exec.LookPath("fzf")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] fzf not found. Falling back to plain text output.\n")
		for _, l := range lines {
			parts := strings.SplitN(l, "\t", 2)
			fmt.Println(parts[0])
		}
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
	cmd.Stdin = strings.NewReader(input)
	cmd.Stderr = os.Stderr
	// RUNEWIDTH_EASTASIAN=0 prevents fzf from treating box-drawing chars as
	// double-width in East Asian locales, which would halve the border width.
	cmd.Env = append(os.Environ(), "RUNEWIDTH_EASTASIAN=0")

	var outBuf strings.Builder
	cmd.Stdout = &outBuf

	err = cmd.Run()
	selected := strings.TrimSpace(outBuf.String())

	if err != nil {
		// Non-zero exit (user pressed ESC = 130, etc.) with no selection is normal.
		// Only fall back to plaintext if fzf itself failed to run.
		if selected == "" {
			return
		}
	}

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
	type candidate struct {
		cmd  string
		args []string
	}
	candidates := []candidate{
		{"wslview", []string{url}},
		{"/mnt/c/Windows/System32/cmd.exe", []string{"/c", "start", "", url}},
		{"/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe", []string{"Start-Process", url}},
	}

	for _, c := range candidates {
		cmd := exec.Command(c.cmd, c.args...)
		cmd.Stdout = nil
		cmd.Stderr = nil
		err := cmd.Run()
		if err == nil {
			return
		}
	}

	fmt.Println("URL:", url)
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

	client, err := api.DefaultGraphQLClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[Error] Failed to create GraphQL client: %v\n", err)
		os.Exit(1)
	}

	// Get authenticated user via GraphQL viewer query.
	var viewerResp struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	err = client.Do("query { viewer { login } }", nil, &viewerResp)
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

	// Parallel fetch.
	var (
		awaiting         []PRNode
		changesRequested []PRNode
		ready            []DashboardItem
		inProgress       []DashboardItem
		wg               sync.WaitGroup
	)

	wg.Add(2)

	go func() {
		defer wg.Done()
		awaiting, changesRequested = fetchPRSections(client, login, *org)
	}()

	go func() {
		defer wg.Done()
		ready, inProgress = fetchProjectItems(client, login, *org)
	}()

	wg.Wait()

	logMsg(fmt.Sprintf("Summary: awaiting=%d changesRequested=%d ready=%d inProgress=%d",
		len(awaiting), len(changesRequested), len(ready), len(inProgress)))

	lines := buildLines(awaiting, changesRequested, ready, inProgress)

	if *dryRun {
		for _, l := range lines {
			parts := strings.SplitN(l, "\t", 2)
			fmt.Println(parts[0])
		}
		if *logPath != "" {
			fmt.Fprintf(os.Stderr, "[Info] Log written to: %s\n", *logPath)
		}
		return
	}

	launchFzf(lines)
}
