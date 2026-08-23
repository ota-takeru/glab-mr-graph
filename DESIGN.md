# glab-mr-graph 設計

## 1. 目的と範囲

`glab-mr-graph` は、GitLab で利用者が関係する open Merge Request（MR）を、
source/target project と branch の関係に基づく有向グラフとして表示する
read-only のローカルツールである。GitHub 向け
[`orangain/gh-pr-graph`](https://github.com/orangain/gh-pr-graph) のグラフ、
レイアウト、埋め込みWeb UI、ローカルサーバーを継承し、GitLab API取得層と
ユーザー向け意味を置き換える。

初版で扱うものは、次の四つの検索集合の和集合である。

```text
scope=created_by_me&state=opened
scope=assigned_to_me&state=opened
scope=reviews_for_me&state=opened
scope=all&search=<title-or-description-query>&state=opened
```

検索結果は GitLab の global MR `id` で重複排除する。検索に直接一致した MR
を seed として、stack の上流・下流を補完する。MR操作、コメント、approve、
assign、merge は提供しない。

初版では Included MR のcommit message検査、未送信レビューコメント、
再レビュー要求の検出を無効化する。Graph DTOに残る旧 Included フィールドは
既存サーバー契約との互換用であり、GitLab loaderは値を生成せず、GitLab UIも
表示しない。

## 2. 実行形態とセキュリティ

```text
glab-mr-graph
├── cmd/glab-mr-graph       CLI、signal、browser起動
├── internal/gitlab          glab api実行、検索、hydrate、stack探索
├── internal/graph           provider非依存のnode/edge構築
├── internal/server          loopback HTTP APIとembedded assets
├── internal/server/web      素のHTML/CSS/JavaScript UI
└── internal/demo            fixture loader
```

- Go 1.23、追加依存なし、単一バイナリを維持する。
- API通信は認証済み `glab` CLIへ委譲する。アプリ自身はtokenを読み取らず、
  ブラウザにも渡さない。
- `glab api` のstdoutはメモリ上だけでdecodeし、disk cacheを持たない。
- API response bodyとstderrを通常ログやOpenTelemetry属性へコピーしない。
- serverは常に `127.0.0.1` へlistenする。静的UIとJSON APIに
  `Cache-Control: no-store` を付ける。
- `--hostname` が指定された場合は `glab api --hostname HOST` を使う。
  空の場合はglabの既定host設定に任せ、GitLab.com/Self-managedの両方に対応する。
- browser起動は `--no-open` で抑止でき、`--port` でportを固定できる。

## 3. GitLab APIクライアント

### 3.1 コマンド実行

`internal/gitlab.Client` は次の小さな境界を持つ。

```go
type CommandRunner interface {
    Run(context.Context, []string) ([]byte, error)
}
```

本番は `glab` subprocess runner、テストは `RunnerFunc` でfixtureを返す。
list endpointには必ず `--paginate` を付ける。GitLab APIのpage配列が複数JSON
値としてstdoutに連結されても、decoderがすべてを結合する。空stdoutやdecode
失敗は安全なエラーとして返す。

### 3.2 検索、detail、approval

起動時に `GET /user` を一回呼び、`username` をviewerとしてrelation判定へ使う。
各検索は個別に実行し、global MR idでdedupeする。検索textはGitLab global MR
APIの `search` パラメータへ渡すため、title/descriptionを対象にできる。

検索後、各MRについて次を取得する。

```text
GET /projects/:target_project_id/merge_requests/:iid
GET /projects/:target_project_id/merge_requests/:iid/approvals
GET /projects/:project_id
```

project responseはprocess-local mapへcacheし、source/target namespace、URL、
default branchを補う。detailが失敗してもlist responseを残す。approvalが
403/404、権限不足、feature unavailableの場合はそのMRを除外せず、approval
数を空またはreviewer数へfallbackする。warningは重複排除してUIへ表示する。

### 3.3 graph DTOへの正規化

既存の `graph.PullRequest` をprovider非依存DTOとして継続利用する。

| graph field | GitLab source |
| --- | --- |
| `ID` | `id` を `gitlab:mr:<id>` としたglobal identity |
| `Number` | `iid` |
| `URL`, title, description | `web_url`, `title`, `description` |
| repository | `target_project_id` + project response |
| head repository | `source_project_id` + project response |
| base/head branch | `target_branch` / `source_branch` |
| draft | `draft` または `work_in_progress` |
| assignees/reviewers | `assignees` / `reviewers` |
| approvals | `/approvals` の `approved_by`, `approvals_required`, `approvals_left` |
| CI | detail/listの `head_pipeline.status` |
| conflict | `detailed_merge_status` / `merge_status` |

Pipeline statusは `SUCCESS`, `FAILURE`, `PENDING`, `SKIPPED`, `UNKNOWN` へ、
conflictは `CONFLICTING` へ正規化する。relationの優先順位は `mine`、
`assigned`、`review-requested`、`other` であり、既存 `graph.RelationFor` を使う。

## 4. stack探索とグラフ意味

探索は検索へ直接一致したMRをseedとしたBFSである。上限はseedを含めて
500 MR、深さ20とし、global MR idとbranch identityのvisited setで重複・循環を
防止する。

### 下流

親MR Aのhead identityを `(A.source_project_id, A.source_branch)` とする。
次のMR Bは次の条件をすべて満たす必要がある。

```text
B.target_project_id == A.source_project_id
B.target_branch     == A.source_branch
```

API queryは `target_project_id` と `target_branch` を使い、レスポンスを同じ
identity条件で再確認する。source projectがforkでも、target projectが一致
しなければ接続しない。

### 上流

子MR Bのbase identityを `(B.target_project_id, B.target_branch)` とする。
親MR Aは次の条件をすべて満たす必要がある。

```text
A.source_project_id == B.target_project_id
A.source_branch     == B.target_branch
A.target_project_id == B.target_project_id
```

`target_project_id` がdefault projectでないMRや、同名branchを持つforkは誤って
親に採用しない。default branchへ戻れないbaseは既存graph builderのbranch nodeへ
接続する。

### レイアウト

repository nodeをrank 0、stacked MRを依存深度ごとのrankへ置き、repositoryごと
にforest laneを作る。グラフbuildはPRという歴史的内部名を保持するが、JSONの
`kind`は既存互換の `pullRequest`、画面の表記は `Merge request` と `!IID` にする。

## 5. UIと更新

- header、title、brand、検索placeholder、empty stateはMR/GitLab表記にする。
- MRカードには `!IID`、title、author、assignees、approval数、pipeline、conflict、
  draft/readyを表示する。
- 背景色はviewerとの関係、枠線はdraft/ready、status rowはapproval/CI/conflict
  を意味し、色だけに依存しない。
- GitLab MR/project/tree URLを新しいtabで開く。
- 検索条件はURLへ保存し、5分polling、manual refresh、visibility/offline停止、
  失敗時backoff、viewport anchor復元を維持する。
- GitLab初版では Included、pending review、re-review UI/API呼び出しを行わない。

## 6. 配布と検証

moduleとbinaryは `github.com/ota-takeru/glab-mr-graph` / `glab-mr-graph` とする。
GitHub Actionsは upstream のCI/release構成を保ち、release buildの出力先だけ
GitLab版binary名へ変更する。MIT licenseとupstream linkをREADMEに明記する。

通常の変更では次を実行する。

```sh
git diff --check
node --check internal/server/web/app.js
go test ./...
go vet ./...
make test
make build
```

fixture testでは、OR dedupe、pagination、JSON変換、approval error、project
cache、relation priority、fork同名branch、stack exact identity、MaxMRs/MaxDepth
を確認する。実GitLab APIを使うテストは通常CIへ含めず、認証情報を要求しない。
