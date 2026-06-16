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
