# gh-dashboard

A `gh` CLI extension that fetches review requests and project items from GitHub's GraphQL API and displays them interactively via `fzf`, opening selected items in the browser.

## Installation

**Prerequisites:** [`gh`](https://cli.github.com/) and [`fzf`](https://github.com/junegunn/fzf) must be installed.

```bash
gh extension install pycabbage/gh-dashboard
```

### Usage

```bash
# Launch across all orgs you belong to
gh dashboard

# Scope to a specific org
gh dashboard --org <org-name>
```

### Sections

| Section | Contents |
|---------|----------|
| Awaiting Approval | PRs where your review has been requested |
| Changes Requested | Your PRs that have received change requests |
| Reviewed by Me | Open PRs you have already reviewed (excluding your own) |
| In Review | Project items assigned to you with "In Review" status |
| Ready | Project items assigned to you with "Ready" status |
| In Progress | Project items assigned to you with "In Progress" status |

### Preview panel

After launching, use arrow keys to switch preview content:

| Key | Content |
|-----|---------|
| default | Details (`gh pr view` / `gh issue view`) |
| `→` | Comments |
| `←` | Repository (`gh repo view`) |

## Development

### セットアップ

```bash
go tool lefthook install
```
