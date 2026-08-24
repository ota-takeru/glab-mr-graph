# glab-mr-graph

`glab-mr-graph` turns GitLab Merge Requests that need your attention into one
local visual workspace. It collects open MRs you authored, were assigned, or
were asked to review, then follows exact source/target project and branch
relationships to show stacked work across projects.

![MR Graph showing related merge requests across projects](docs/images/screenshot.png)

The project is a GitLab-oriented fork of
[`orangain/gh-pr-graph`](https://github.com/orangain/gh-pr-graph). The graph
layout and local server retain the upstream implementation where that keeps
the behavior stable; the provider, API mapping, UI wording, and release
artifacts are GitLab-specific.

## Requirements

- [GitLab CLI (`glab`)](https://gitlab.com/gitlab-org/cli), authenticated with
  `glab auth login`
- Go 1.23 or newer when building from source

The authenticated `glab` configuration supplies the token for GitLab.com or a
self-managed GitLab instance. Tokens and API responses are never passed to
the browser or written to disk.

## Install and run

The installer downloads the release binary into your user account and registers
a managed `glab` shell alias. Review the linked installer before piping remote
code into a shell. On macOS, Linux, or FreeBSD:

```sh
curl -fsSL https://raw.githubusercontent.com/ota-takeru/glab-mr-graph/main/scripts/install.sh | sh
glab mr-graph
```

On Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/ota-takeru/glab-mr-graph/main/scripts/install.ps1 | iex
glab mr-graph
```

The POSIX installer uses `$XDG_BIN_HOME`, or `~/.local/bin` when it is unset.
The Windows installer uses `%LOCALAPPDATA%\glab-mr-graph\bin` and adds that exact
directory to the user `PATH`. Git for Windows is not required: when `glab`
cannot use an existing shell, the installer adds a narrowly scoped compatibility
`sh.exe` under the private `runtime` directory and appends that directory to
the user `PATH`. The shim accepts only the managed `mr-graph` alias; it does not
interpret arbitrary shell commands. An existing shell remains ahead of the
private runtime and is preferred. Override either install location with
`GLAB_MR_GRAPH_INSTALL_DIR`. Set `GLAB_MR_GRAPH_VERSION` to a published release
tag matching `vX.Y.Z` to pin a version; the default is the latest release.
Running the installer again upgrades the binary and managed shim in place.

The installer refuses to replace an existing `mr-graph` alias or shell runtime
unless it is marked as managed by this installation. Downloads complete in
temporary files before replacement; a download or alias registration failure
restores both the previous binary and previous shim.

To uninstall on macOS, Linux, or FreeBSD:

```sh
curl -fsSL https://raw.githubusercontent.com/ota-takeru/glab-mr-graph/main/scripts/uninstall.sh | sh
```

On Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/ota-takeru/glab-mr-graph/main/scripts/uninstall.ps1 | iex
```

Uninstall removes only the managed alias, binary, and compatibility shim at the
configured install path. On Windows it removes `PATH` entries only when they
were added by the installer. Pre-existing shells, `PATH` entries, and unrelated
`mr-graph` aliases are preserved.

You can also download a release asset named for the target platform, or build
the standalone binary locally:

```sh
make build
./glab-mr-graph
```

The server listens only on `127.0.0.1` and opens the graph in your browser.
Press Ctrl-C to stop it.

```text
--no-open          print the URL without opening a browser
--port 8080        override the preferred local port (8787)
--hostname HOST    use a GitLab.com or self-managed GitLab hostname
```

For example:

```sh
glab mr-graph --hostname gitlab.example.com --no-open
```

The Authored, Assigned, and Review requested filters are OR conditions. The
search field adds a fourth OR condition and searches MR titles and
descriptions. Clear all three relationship filters to use only the text
search.

MR node backgrounds show why an MR is in the workspace: blue means authored,
cyan means assigned, green means review requested, and gray means related
stack context. Draft/ready state, approval state/counts, pipeline status, and
merge conflicts are shown independently. Approval availability is kept separate
from reviewer assignment. The UI is read-only in this first GitLab
version; Included MR history, pending-review comments, and re-review markers
are intentionally disabled.

## Demo mode

Use the built-in fixture data to work on the UI without querying GitLab:

```sh
GLAB_MR_GRAPH_DEMO=1 ./glab-mr-graph
```

Demo mode is enabled only through `GLAB_MR_GRAPH_DEMO`; it is not a CLI option.

## Development

```sh
make test
make build
```

Installer tests use a fake `glab` and local release fixtures; they never need
GitLab credentials or a network download:

```sh
bash scripts/install_test.sh
pwsh -File scripts/install_test.ps1
```

`GLAB_MR_GRAPH_ASSET_PATH` and `GLAB_MR_GRAPH_SHIM_ASSET_PATH` supply local
fixture binaries for these tests.

The provider lives in `internal/gitlab`. It invokes `glab api` with manual
`per_page=100&page=N` pagination, stops each search spec at 500 items, and
deduplicates global MR IDs across the relationship searches. It hydrates MR
details and approvals, keeps a process-local project cache with per-project
singleflight, and bounds each
refresh to a 2-minute context, 30-second requests, and 1,200 `glab`
subprocesses. The command runner is injectable so API fixtures can be tested
without a token.

The server streams the list-derived stack topology first, with approvals marked
as loading, and renders it immediately. It then hydrates all MR details and
approvals with at most six workers and streams a complete status update. If a
per-MR status request is unavailable or exhausts the refresh budget, the
topology remains visible and the affected status is shown as unavailable.

Stack discovery is breadth-first and bounded to 500 MRs and 20 levels. Branch
lists request only the remaining MR capacity. Up to four search specs, branch
queries in one breadth-first frontier, project metadata lookups, and the final
MR hydration run with at most six workers; results and progress are applied in
stable input order. A relationship is accepted only
when the project ID and branch name both match; same-named branches in forks
cannot create an edge.

For design decisions and data-flow limits, see [DESIGN.md](DESIGN.md). For a
GitHub-to-GitLab comparison and current implementation boundaries, see
[docs/github-gitlab-differences.md](docs/github-gitlab-differences.md).

## Tracing

Set `GLAB_MR_GRAPH_TRACE_OTEL=1` to export optional OpenTelemetry traces to
`http://localhost:4318/v1/traces`. Set the variable to an explicit collector
URL to use a different endpoint. Tracing is best effort and does not include
API response bodies, tokens, search text, hostnames, API endpoints, command
arguments, project/MR identifiers, or branch names. Span names and attributes
are limited to fixed operation names, booleans, statuses, and counts. Failed
spans export status code 2 with a fixed `error.type`; external error messages
are excluded. The Go tests cover both recording-tracer redaction and the
encoded OTLP payload.

## Releases

GitHub Actions runs JavaScript checks, tests, vet, cross-platform builds, and
installer tests on Ubuntu and Windows.
User-facing changes are recorded under `Unreleased` in
[CHANGELOG.md](CHANGELOG.md). Release preparation follows the upstream
workflow and produces artifacts based on the `glab-mr-graph` binary name.

## License and attribution

This project is MIT licensed. See [LICENSE](LICENSE). It is derived from
[`orangain/gh-pr-graph`](https://github.com/orangain/gh-pr-graph), also MIT
licensed; the upstream copyright and license notice are retained.
