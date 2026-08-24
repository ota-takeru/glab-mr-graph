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
- API response bodyとstderrを通常ログやOpenTelemetry属性へコピーしない。OpenTelemetryの
  span名と属性も低カーディナリティの固定値・状態・件数だけに限定し、検索語、hostname、
  API endpoint、project/MR ID、branch、`glab` command argsを含めない。span失敗時は
  OTLPへstatus code 2と固定の`error.type`だけを出し、外部errorのmessageは出さない。
- serverは常に `127.0.0.1` へlistenする。静的UIとJSON APIに
  `Cache-Control: no-store` を付ける。
- `--hostname` が指定された場合は `glab api --hostname HOST` を使う。
  空の場合はglabの既定host設定に任せ、GitLab.com/Self-managedの両方に対応する。
- browser起動は `--no-open` で抑止でき、`--port` でportを固定できる。
- デモは `GLAB_MR_GRAPH_DEMO=1`、開発用traceは
  `GLAB_MR_GRAPH_TRACE_OTEL=1`（またはcollector URL）だけで有効にする。

## 3. GitLab APIクライアント

### 3.1 コマンド実行

`internal/gitlab.Client` は次の小さな境界を持つ。

```go
type CommandRunner interface {
    Run(context.Context, []string) ([]byte, error)
}
```

本番は `glab` subprocess runner、テストは `RunnerFunc` でfixtureを返す。
list endpointは `--paginate` を使わず、`per_page=100&page=N` を付けた
subprocessをページ単位で反復する。呼び出し元のlimitに達した時点で次ページを
取得せず、結果と `truncated` を返す。空stdoutやdecode失敗は安全なエラーとして
返す。

### 3.2 検索、detail、approval

起動時に `GET /user` を一回呼び、`username` をviewerとしてrelation判定へ使う。
`GET /user` と各検索は最大6 workerの範囲で並行し、検索結果はglobal MR idでdedupeする。検索textはGitLab global MR
APIの `search` パラメータへ渡すため、title/descriptionを対象にできる。

検索とstack探索では、最初にproject metadataだけを補い、各MRのdetailとapprovalは
topology確定後に取得する。

```text
GET /projects/:target_project_id/merge_requests/:iid
GET /projects/:target_project_id/merge_requests/:iid/approvals
GET /projects/:project_id
```

project responseはprocess-local mapへcacheし、source/target namespace、URL、
default branchを補う。同じproject IDのcache missが並行した場合は1件のAPI呼び出しを
共有し、失敗した呼び出しはcacheせず次回に再試行できるようにする。待機側のcontextが
終了した場合は共有呼び出しの完了を待たずに戻る。detailが失敗してもlist responseを残す。approvalが
403/404、権限不足、feature unavailableの場合はそのMRを除外せず、
`approvalState=UNAVAILABLE` として表示する。reviewer指定はapproval ruleの
代替には使わない。warningは重複排除してUIへ表示する。

`LoadProgress` はデフォルト2分のrefresh contextを作り、各 `glab` subprocessには
デフォルト30秒のrequest timeoutを適用する。1 refreshあたりのsubprocess数は
デフォルト1200件で、budget到達時は `/user` と検索ではエラー、stack探索では
warningを返して停止し、MR単位のdetail・approvalでは既存のdegradeを続ける。

`LoadStages` は同じrefresh contextとrequest budgetの中で二段階のgraphを返す。
最初の `topology` stageはlist responseとproject metadataだけから構築し、approvalを
`LOADING` とする。このstageをNDJSONでflushした後、全MRのdetailとapprovalを一度の
最大6 worker hydrateへ渡し、`complete` stageを返す。status hydrate中にbudgetが尽きても
topologyは取り消さず、該当statusを `UNAVAILABLE` にdegradeしたcomplete graphとwarningを
返す。従来の `Load` / `LoadProgress` は同じcoreのcomplete resultだけを返す。

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
| approvals | `/approvals` の `approved`, `approved_by`, `approvals_required`, `approvals_left` |
| CI | detail/listの `head_pipeline.status` |
| conflict | `has_conflicts` または `detailed_merge_status=conflict` |

Pipeline statusは `SUCCESS`, `FAILURE`, `PENDING`, `SKIPPED`, `UNKNOWN` へ、
`has_conflicts=true` または `detailed_merge_status=conflict` の場合だけ
conflictを `CONFLICTING` へ正規化する。`unchecked`、`checking`、
`cannot_be_merged`、`need_rebase` は競合確定とは扱わない。relationの優先順位は `mine`、
`assigned`、`review-requested`、`other` であり、既存 `graph.RelationFor` を使う。

approvalは `approvalState`、`approvalRequired`、`approvalRemaining`、
`approverCount` へ正規化する。stateは `UNAVAILABLE`（取得失敗またはnil）、
`NOT_REQUIRED`（requiredが0以下）、`APPROVED`（APIの `approved=true` または
required>0かつremaining<=0）、それ以外の `PENDING` とし、取得途中用に `LOADING`
と未知値用の `UNKNOWN` も許容する。`approved_by` の件数はapproverCountとして
表示するが、rule達成の判定には使わない。旧 `ReviewApproved`/`ReviewTotal` は
互換用に残すだけで、UIは新しいapproval fieldsを使う。

## 4. stack探索とグラフ意味

探索は検索へ直接一致したMRをseedとしたBFSである。上限はseedを含めて
500 MR、深さ20とし、global MR idとbranch identityのvisited setで重複・循環を
防止する。各検索specのlistは最大500件で停止し、branch listは残りのMR枠だけを
取得する。最大4件の検索specは6 worker以内で並列実行し、spec順に結果をmergeする。
stack探索も同じdepthの重複しないbranch queryを6 worker以内で並列実行し、その
frontierで見つけたMRをglobal IDで重複排除してproject metadataだけを補う。
全MRのdetail・approvalはtopology stageの配信後に一括hydrateする。
結果の適用とprogress通知はworker外で決定的な順序に行う。直接検索seedは上流・下流の両方向を探索するが、上流で見つけた
親MRは上流だけ、下流で見つけた子MRは下流だけを探索する。これにより、
seedの親から親の別childへ横展開しない。

### 下流

親MR Aのhead identityを `(A.source_project_id, A.source_branch)` とする。
次のMR Bは次の条件をすべて満たす必要がある。

```text
B.target_project_id == A.source_project_id
B.target_branch     == A.source_branch
```

API queryは `/projects/:source_project_id/merge_requests?target_branch=...`
を使い、レスポンスを同じidentity条件で再確認する。source projectがforkでも、
target projectが一致しなければ接続しない。

### 上流

子MR Bのbase identityを `(B.target_project_id, B.target_branch)` とする。
親MR Aは次の条件をすべて満たす必要がある。

```text
A.source_project_id == B.target_project_id
A.source_branch     == B.target_branch
```

API queryは `/merge_requests?scope=all&state=opened&source_branch=...` を使い、
レスポンスの `source_project_id` と `source_branch` を子のbase identityと再確認する。
親の `target_project_id` は子のtarget projectと異なっても、source projectとbranchが
一致するvalidなcross-project forkとして採用する。同名branchを持つ別forkはsource
projectが一致しないため採用しない。default branchへ戻れないbaseは既存graph
builderのbranch nodeへ接続する。project metadataからdefault branchを取得できない
場合はbase branchをdefault branchと推測せず、上流queryを発行しない。

### レイアウト

repository nodeをrank 0、stacked MRを依存深度ごとのrankへ置き、repositoryごと
にforest laneを作る。グラフbuildはPRという歴史的内部名を保持するが、JSONの
`kind`は既存互換の `pullRequest`、画面の表記は `Merge request` と `!IID` にする。

## 5. UIと更新

- header、title、brand、検索placeholder、empty stateはMR/GitLab表記にする。
- MRカードには `!IID`、title、author、assignees、approval state/count、pipeline、conflict、
  draft/readyを表示する。
- 背景色はviewerとの関係、枠線はdraft/ready、status rowはapproval/CI/conflict
  を意味し、色だけに依存しない。
- GitLab MR/project/tree URLを新しいtabで開く。
- 検索条件はURLへ保存し、5分polling、manual refresh、visibility/offline停止、
  失敗時backoff、viewport anchor復元を維持する。browserのgraph fetchは
  125秒でabortし、timeoutを明示する。
- NDJSONの `topology` resultを受信した時点でgraphを描画し、`complete` resultでstatusを
  更新する。各resultは同じfilterとviewport anchor復元を通す。中間描画後にstream errorが
  届いた場合も表示済みgraphを残してerrorを表示する。
- GitLab初版では Included、pending review、re-review UI/API呼び出しを行わない。

## 6. 配布と検証

moduleとbinaryは `github.com/ota-takeru/glab-mr-graph` / `glab-mr-graph` とする。
GitHub Actionsは upstream のCI/release構成を保ち、release buildの出力先だけ
GitLab版binary名へ変更する。MIT licenseとupstream linkをREADMEに明記する。

GitHub向けupstreamの `gh extension install` に近い操作感として、POSIXの
`scripts/install.sh` とWindows PowerShellの `scripts/install.ps1` を提供する。
installerはOS/architectureに対応するGitHub Releaseのraw assetをユーザー領域へ
downloadし、`glab alias set --shell mr-graph` で `glab mr-graph` を登録する。
POSIXは `$XDG_BIN_HOME` または `$HOME/.local/bin`、Windowsは
`%LOCALAPPDATA%\glab-mr-graph\bin` を既定とし、`GLAB_MR_GRAPH_INSTALL_DIR` で
上書きできる。WindowsはGit for Windowsの `sh` をdownload前に必須確認し、install先を
user PATHへ追加する。installerが追加したPATHだけをmarkerで識別してuninstall時に戻す。

`GLAB_MR_GRAPH_VERSION` は既定の `latest` または厳密な `vX.Y.Z` を受け入れる。
`GLAB_MR_GRAPH_ASSET_PATH` はnetworkを使わないfixture test専用のlocal asset override
である。downloadはinstall先と同じdirectoryの一時fileへ完了してからbinaryを置換し、
download失敗時は既存binaryを保持する。再実行は同じpathのbinaryをupgradeし、alias登録に
失敗した場合もbackupしたbinaryへrollbackする。

install/uninstallは `glab alias list` のcommandをinstallerが生成する値と完全一致で確認する。
別用途の `mr-graph` aliasがある場合、installは衝突として停止し、uninstallはそのaliasを
保持する。uninstallerが削除する実行fileは選択されたinstall directory直下の
`glab-mr-graph`（Windowsは `.exe`）だけで、directoryや他のfileは再帰削除しない。
Ubuntuではfake `glab` とlocal POSIX asset、Windowsではfake `glab` とlocal Windows assetを
使ってalias、引数転送、latest/version pin、install directory override、upgrade、失敗時保持、
衝突、uninstall、PATH追加・削除をCIで検証する。

通常の変更では次を実行する。

```sh
git diff --check
node --check internal/server/web/app.js
go test ./...
go vet ./...
make test
make build
bash scripts/install_test.sh
pwsh -File scripts/install_test.ps1
```

fixture testでは、OR dedupe、検索・branch listの並列数と決定的merge、全MR一括
hydrate、manual paginationのpage停止とtruncated、JSON変換、approval error、
project cache/singleflight、relation priority、fork同名branch、stack exact
identity、MaxMRs/MaxDepth、request budget、request/refresh timeoutを確認する。
trace testでは、Client/serverのStart/End属性とspan名に検索語、hostname、endpoint、
project/MR ID、branchが入らないこと、および実OTLP payloadがerror messageを含まず
status code 2と固定`error.type`だけを出すことを確認する。
段階配信ではtopologyより前にdetail/approvalを呼ばないこと、topology/completeの
node・edge identity、status hydrateのdegrade、chunk分割されたNDJSONを確認する。
実GitLab APIを使うテストは通常CIへ含めず、認証情報を要求しない。
