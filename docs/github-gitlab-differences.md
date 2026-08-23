# GitHub版とGitLab版の実装差分

## 対象と現状スナップショット

この文書は、upstream の [`orangain/gh-pr-graph`](https://github.com/orangain/gh-pr-graph) と、そこから派生した `glab-mr-graph` の違いを、現在のコードに基づいて整理する。参照会話で挙がった候補や将来案を、実装済みの機能として扱わない。

現行版の位置づけは次のとおりである。

| 観点 | `gh-pr-graph` | 現行 `glab-mr-graph` |
| --- | --- | --- |
| 対象 | GitHub の open Pull Request | GitLab の open Merge Request |
| 取得境界 | `gh` CLI 経由の GitHub API | `glab` CLI 経由の GitLab REST API |
| ワークスペース | 作成者、assignee、review request と検索結果を横断表示 | 作成者、assignee、review request とテキスト検索結果を OR で横断表示 |
| グラフ | repository/branch と PR の stacked 関係 | target/source project と branch の厳密な MR 関係 |
| 状態表示 | GitHub の review、checks、merge 状態を正規化 | approval 数、`head_pipeline`、確定した conflict を個別に表示 |
| 操作 | ローカルの read-only グラフ | ローカルの read-only グラフ。コメント、approve、assign、merge は行わない |
| 互換境界 | upstream の PR 向け DTO/UI | グラフ・サーバー・画面の共通部分を残し、取得と意味付けを GitLab 用に置換 |

したがって、これは GitHub の機能をすべて GitLab の同じ名前へ置き換えた完全互換版ではない。GitLab の MR を、複数 project をまたぐ read-only workspace として俯瞰するための初版である。

## upstreamから維持する共通コア

GitLab 対応で provider 部分を置き換えたが、次の責務は upstream の構造と共通である。

| 共通コア | 現行コードでの役割 |
| --- | --- |
| `internal/graph` の DTO | `graph.PullRequest`、`Node`、`Edge`、`Result` を JSON 契約として継続利用する。`PullRequest` という歴史的な型名、JSON の `kind: "pullRequest"`、`number` なども互換性のため残す |
| `graph.Build` | repository node、未解決 base branch node、MR node を rank 付けし、project/repository と branch の identity から edge を作る |
| `server.Loader` 境界 | provider の取得結果をローカル HTTP API へ渡す境界として維持する |
| embedded Web UI | 追加依存なしの HTML/CSS/JavaScript を Go バイナリへ埋め込み、同じグラフ描画、lane layout、edge 描画を使う |
| 更新体験 | URL に検索条件を保存し、manual refresh、5 分 polling、失敗時 backoff、visibility/offline 停止、viewport anchor 復元を維持する |
| ローカル実行と保護 | server は `127.0.0.1` のみで listen し、認証情報と API response body をブラウザや disk cache へ渡さない |

共通 core の再利用により、GitLab 版で書き直した中心は `internal/gitlab` の API client、GitLab の正規化、stack 探索、MR 向けの表記と URL である。GitLab 用の provider であることは DTO の `provider: "gitlab"` と画面の MR 表記で表し、共通 DTO の名前を無理に変更していない。

## 実装済みのGitLab差分

### API境界: `glab`、REST、pagination、hostname

- 認証はアプリが token を読むのではなく、認証済み `glab` subprocess に委譲する。通常の API 呼び出しは `glab api` で行う。
- 一覧 endpoint には `--paginate` と `per_page=100` を付ける。`glab api --paginate` が複数の JSON 配列を改行で返しても、decoder が全 page を結合する。
- detail、approval、project は個別 REST endpoint を使う。GitHub 版の GraphQL query を移植してはいない。
- `--hostname HOST` を指定したときは `glab api --hostname HOST` として渡す。空なら `glab` の設定に任せ、GitLab.com と self-managed GitLab の両方を同じ client で扱う。
- response body や stderr は通常ログへ出さない。空 response や decode 失敗はエラーとして扱う。

これは「REST だから単純に同じ request になる」という意味ではない。GitLab 版は一覧、detail、approval、project を別々に取得するため、API 回数と利用可能な field の差を client 側で吸収している。

### 4集合のOR検索とglobal id dedupe

UI の三つの関係フィルターと任意のテキスト検索を、次の四つの独立した検索として実行する。

| UI 条件 | GitLab REST の条件 |
| --- | --- |
| Authored | `scope=created_by_me&state=opened` |
| Assigned | `scope=assigned_to_me&state=opened` |
| Review requested | `scope=reviews_for_me&state=opened` |
| Text search | `scope=all&search=<text>&state=opened` |

結果は単一の GitLab global MR `id` を優先して `gitlab:mr:<id>` に正規化し、検索集合の和集合を map で重複排除する。表示番号は project 内の `iid` である。`reviews_for_me` に直接一致した ID は、同じ MR が他の集合にも現れた場合でも review-requested の関係情報を失わない。

テキスト検索は GitHub の検索 DSL を解釈せず、GitLab の `search` parameter に渡す title/description 検索である。これは検索仕様を完全に同じにしたものではない。

### project + branch の厳密な stack、BFS 上限、探索方向

検索に直接一致した MR を seed とし、stack の補完は breadth-first search で行う。デフォルト上限は seed を含めて 500 MR、深さ 20 である。project ID と branch の組を visited key として使い、循環や同じ branch の再探索を抑える。

下流の候補は、親 MR `A` と子 MR `B` について次をすべて満たす場合だけ採用する。

```text
B.target_project_id == A.source_project_id
B.target_branch     == A.source_branch
```

上流の候補は、子 MR `B` と親 MR `A` について次をすべて満たす場合だけ採用する。

```text
A.source_project_id == B.target_project_id
A.source_branch     == B.target_branch
A.target_project_id == B.target_project_id
```

API query の branch filter だけに依存せず、response を同じ identity 条件で再確認する。そのため、fork にある同名 branch や、target project が異なる MR を stack の edge として誤採用しない。

探索方向も制限する。直接検索 seed は上流・下流の両方を見るが、上流で見つかった MR は上流だけ、下流で見つかった MR は下流だけを引き続き見る。親を起点に親の別 child へ横展開することはない。上限到達時は graph を返しつつ warning を出し、検索を狭めるよう促す。

### 並列 hydrate、project cache、graceful degradation

- 四つの検索条件と branch ごとの stack 探索は現在は逐次実行する。GitHub 版の並列検索や GraphQL alias による同一 depth の batch query は移植していない。
- 検索で得た MR の detail と approvals は最大 6 worker で並列に hydrate する。サーバーの refresh 自体は直列化し、polling と manual refresh が API cost を重ねない。
- source/target project の namespace、URL、default branch を補う project lookup は process-local map に cache する。cache は disk に保存しない。
- detail が失敗した場合は list response を残し、その MR を落とさない。project lookup が失敗しても、利用できる ID から fallback の表示情報を作る。
- approval endpoint が権限不足、404、feature unavailable などで失敗しても MR は表示し、reviewer 数を fallback として使う。warning は重複排除して UI へ渡す。
- 検索、`/user`、stack branch list など graph の入口に必要な request の失敗は、同じ意味で黙って成功扱いにはしない。graceful degradation の対象は主に MR 単位の detail、approval、project 補足である。

### Approval 数、`head_pipeline`、確定 conflict

GitHub の per-reviewer review decision を再現するのではなく、GitLab MR の `/approvals` を集約表示へ正規化する。

| 表示値 | 現在の根拠 |
| --- | --- |
| approved | `/approvals` の `approved_by` 件数。必要数と残数から補える場合は `approvals_required - approvals_left` を使う |
| total | `approvals_required`。値が取れない、または 0 の場合は reviewer 件数へ fallback |
| review decision | 必要 approval 数があり `approvals_left <= 0` の場合だけ `APPROVED`。それ以外の reviewer lifecycle は表さない |
| CI | `head_pipeline.status` を優先し、なければ `pipeline.status` を使う。success/failed/canceled/pending/running/skipped 等を共通状態へ正規化 |
| conflict | `has_conflicts=true` または `detailed_merge_status=conflict` の場合だけ `CONFLICTING`。`checking`、`unchecked`、`cannot_be_merged`、`need_rebase` は確定 conflict としない |

画面では `!IID`、approval 数、pipeline status、conflict、draft/ready を別々の status row として表示する。MR の `web_url` をそのままリンクに使い、project URL と `/-/tree/<branch>` URL も GitLab 形式で生成する。MR、project、branch のリンクは新しい tab で開く。

## GitHub側との意味差

| 領域 | GitHub 側の意味 | GitLab 側の意味と現行の扱い |
| --- | --- | --- |
| 検索 | GitHub PR search の query syntax で author、label、review などを組み合わせられる | REST の scope と `search` parameter が中心。現行 UI は三つの scope + title/description の text search だけ |
| ID | GraphQL global node ID が dedupe/identity、画面番号は repository 内の PR number | global MR `id` が dedupe/identity、画面番号は project 内 `iid`。stack では source/target project ID も必須 |
| Reviewer と approval | review request、latest review、`reviewDecision` などを比較的直接に統合できる | reviewer 指定と approval rule/approval 数が別概念。現行版は reviewer list と aggregate approval 数だけを出す |
| Changes requested / pending / re-review | GitHub 版が扱うレビュー lifecycle の状態 | GitLab 版では対応する履歴や個人単位状態を取得せず、DTO に残る旧フィールドも生成しない |
| CI | checks/status の rollup を PR の状態として扱える | MR に紐づく head pipeline の状態だけ。pipeline 成功は、security policy、external status check、discussion、approval などを含む merge 可否とは同じではない |
| Merge 状態 | GitHub の mergeable/merge state を別の状態として扱う | `detailed_merge_status` は非同期かつ多くの blocker を含む。現行版は誤検知を避け、確定 conflict のみを conflict 表示へ反映する |
| Included | commit history と associated PR を使って Included PR を求められる | commit message/関連 MR の検査をまだ行わない。共通 DTO の Included フィールドは契約互換のため残るだけ |
| Stack | repository + branch の対応をたどる | fork を含む source/target project と branch の条件を各方向で検証し、より厳密に edge を作る。GitLab native stack UI や `glab stack` の状態は利用しない |
| API と性能 | GraphQL で必要 field をまとめ、検索を並列化し、同一 depth の branch query を alias で batch 化できる | REST の list/detail/approval/project を分けて取得する。検索と stack 探索は逐次、detail/approval hydrate は最大 6 並列とし、project cache、上限、warning で request cost と欠損を制御する |

この差により、GitLab の `reviewer` を GitHub の `reviewDecision` と同じ意味で表示したり、`head_pipeline=success` を「merge 可能」と表示したりすることはできない。現行の共通 DTO 名はデータの意味が完全一致することを宣言するものではなく、既存 graph/server/UI 契約を維持するための adapter 境界である。

## 未実装・意図的に無効なもの

| 領域 | 現状 | 理由・境界 |
| --- | --- | --- |
| 検索 DSL | GitHub query syntax は解釈しない | GitLab REST の text search と scope の意味を偽って GitHub DSL として受け付けない |
| structured filters | project、author、reviewer、label、state などを独立した検索条件として追加していない | 初版の四集合 OR を越える UI/API 契約は未定義 |
| per-reviewer lifecycle | pending、commented、changes requested、最新 review の個人単位状態を取得しない | GitLab の reviewer と approval rule を一つの `ReviewDecision` に押し込めないため |
| detailed approval rules | approval rule、eligible approver、rule ごとの不足数を取得しない | tier、権限、GitLab version による利用可能性が異なり、現行の数値表示を越えるため |
| pending / re-review | pending review、未送信コメント、修正後の再レビュー要求を検出しない | GitLab API の追加履歴取得と時系列判定をまだ導入していない。Graph DTO の旧フィールドは空のまま |
| Included MR | commit message 検査、関連 MR lookup、Included MR の UI/API は GitLab loader で実装しない | 共通 server に残る互換用 endpoint は GitLab loader では `501 Not Implemented` になり、機能があるようには見せない |
| merge blocker 全般 | conflict 以外の CI、approval、discussion、security policy、external status check、merge train 等を blocker として列挙しない | `detailed_merge_status` の値を完全な merge 可否へ誤変換しない。pipeline と conflict は独立表示する |
| native stack / `glab stack` 連携 | GitLab UI の native stacked MR や `glab stack` の作成・並べ替え・sync 状態を読まない、書かない | 本ツールは既存 MR の関係を横断表示する read-only workspace であり、stack manager ではない |
| 書込操作 | comment、approve、assign、merge、rebase、stack 操作などを提供しない | 認証情報と副作用をローカル read-only 境界に閉じ込める |
| progressive two-phase hydration | list の topology を先に表示し、後から detail/approval を段階的に差し込む方式は未実装 | 現在は検索結果を detail/approval hydrate してから graph を完成させる。進捗イベントはあるが、中間 graph を返す契約ではない |

## 現方針と将来追加の条件

現行の方針は、GitLab の stack manager を再実装することではなく、GitLab の複数 project にまたがる MR を一画面で読むための横断 read-only workspace を提供することである。GitLab が持つ native stack や `glab stack` と同じ lifecycle を作るのではなく、作成方法に依存せず source/target project と branch の関係を検出する。

そのため、GitHub 版との「完全互換」を名目に、意味の異なる GitLab の値を同じラベルへ詰め込まない。特に reviewer lifecycle、approval rule、merge blocker は、利用可能な情報だけを明示的な独立状態として扱い、分からないものは unknown、warning、未表示のままにする。GitLab.com/self-managed の tier、権限、version 差があっても、MR 全体を消さずに利用可能な list data へ degrade できることを優先する。

`graph.PullRequest`、`reviewDecision`、`IncludedPRs` などの既存名は、GitLab の概念が GitHub と同一だからではなく、graph/server/UI の既存 JSON 契約を壊さないために保持する。新しい意味を追加する場合は、既存 field の意味を密かに変えず、provider を表す情報や独立した field を検討する。

将来、検索条件の拡張、per-reviewer review 状態、詳細 approval rules、merge blockers、Included MR、二段階 hydration、native stack 連携を追加する場合は、少なくとも次を満たすことを条件とする。

1. GitLab.com と主要な self-managed/version/tier の差を確認し、取得できない場合の fallback と warning を定義できること。
2. GitHub の既存概念との対応が一対一でない場合に、UI と DTO で別の意味として説明でき、誤った完全互換を作らないこと。
3. refresh の API 回数、並列数、レート制限、待ち時間を上限付きで評価できること。中間表示を導入するなら、progress event ではなく明確なデータ契約と UI 状態を追加すること。
4. fork、同名 branch、権限不足、feature unavailable、非同期 merge status を fixture test で再現し、欠損時にも安全に表示できること。
5. 読み取り専用の境界と、認証情報・private response をブラウザ、永続 cache、通常ログへ漏らさない制約を維持できること。

詳細なデータフロー、上限、cache、trace、UI の状態定義は [`DESIGN.md`](../DESIGN.md) を参照し、この文書は GitHub 版との差分と判断理由の入口として使う。
