# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# ビルド
go build -o gh-dashboard .

# 実行（gh extension として）
gh dashboard
gh dashboard --org <org-name>

# dry-run（fzf を起動せずプレーンテキスト出力）
./gh-dashboard --dry-run
./gh-dashboard --org <org-name> --dry-run

# デバッグログ出力（--dry-run と組み合わせ可能）
./gh-dashboard --log /tmp/gh-dashboard.log --dry-run

# GraphQL コード生成（queries.graphql を変更した後に実行）
go generate ./gql/...

# スキーマ再取得（go generate の前に必要な場合）
curl --fail-with-body https://docs.github.com/public/fpt/schema.docs.graphql -o gql/schema.graphql

# 依存関係の整理
go mod tidy
```

## アーキテクチャ

### 全体像

`gh` CLI extension として動作する単一バイナリ。GitHub GraphQL API からデータを並列取得し、`fzf` でインタラクティブに表示する。

```
main.go          ← エントリポイント・全ビジネスロジック
gql/
  queries.graphql  ← GraphQL クエリ定義（編集対象）
  genqlient.yaml   ← コード生成設定
  generate.go      ← //go:generate ディレクティブのみ
  schema.graphql   ← GitHub GraphQL スキーマ（gitignored, 自動生成）
  generated.go     ← 生成された型・関数（gitignored, 自動生成）
```

### GraphQL コード生成（genqlient）

`gql/queries.graphql` にクエリを追加・変更したら `go generate ./gql/...` を実行する。`gql/generated.go` が自動更新される（コミット不要、gitignore 済み）。

`Issue.state` と `PullRequest.state` は同名フィールドだが型が異なるため、クエリ内で `issueState: state` / `prState: state` のエイリアスで分離している。

### データフロー

1. `api.NewHTTPClient` (go-gh) で認証済み HTTP クライアント生成 → `graphql.NewClient` に渡す
2. `fetchPRSections` と `fetchProjectItems` を goroutine で並列実行
3. `fetchPRSections`: 1クエリで `awaitingApproval`（レビュー依頼）・`changesRequested`（自分のPR、`review:changes_requested` で絞り込み済み）を同時取得
4. `fetchProjectItems`: `--org` 指定あり → `FetchOrgProjectItems`、なし → `FetchViewerOrgProjectItems`（viewer の全所属 org を対象）
5. `buildLines` で fzf 用の `{display}\t{url}` 形式の行を構築 → `launchFzf` に渡す

### projectItemData パターン

genqlient は クエリごとに異なる Go 型を生成するため、`FetchOrgProjectItems` と `FetchViewerOrgProjectItems` の item 型は別物。`processOrgProjectItem` / `processViewerOrgProjectItem` がそれぞれ型 switch でフィールドを抽出し、共通の `projectItemData` struct に詰め替えた後、`classifyProjectItem` で統一処理する。

### プロジェクトアイテムのフィルタ条件

- `state == OPEN`
- 自分がアサインされている
- `Status` フィールドの値が `"ready"` または `"in progress"` を含む（case-insensitive）

### fzf のプレビューパネル

fzf 起動後、矢印キーでプレビュー内容を切り替えられる：

| キー | 表示内容 |
|------|---------|
| デフォルト | Details（`gh pr view` / `gh issue view`） |
| `→` | Comments（`gh pr view --comments` 等） |
| `←` | Repository（`gh repo view`） |

プレビューコマンドは `GH_FORCE_TTY=1`・`GLAMOUR_STYLE=dark`・`CLICOLOR_FORCE=1` を付与して実行することで、パイプ経由でも markdown レンダリングが有効になる。

### fzf 起動の注意点

日本語ロケール（`ja_JP.UTF-8`）では `mattn/go-runewidth` がボーダー罫線文字を幅2として扱い表示が崩れるため、`RUNEWIDTH_EASTASIAN=0` を環境変数に付与して起動する。

### ブラウザ起動

`github.com/cli/browser` パッケージ（`browser.New`）に委譲しており、WSL2 でのブラウザ起動もそのパッケージが処理する。`Browse` が失敗した場合のみ URL をそのまま標準出力に表示する。
