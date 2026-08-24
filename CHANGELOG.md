# Changelog

Notable user-facing changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.15.0] - 2026-08-24

- Port the graph workspace to GitLab as `glab-mr-graph`, including authenticated `glab api` search, merge-request hydration, approval/pipeline status mapping, and exact project/branch stack discovery.
- Bound GitLab refresh cost with pagination, request/refresh timeouts, and a subprocess budget; add correct approval-unavailable handling, cross-project fork stacks, parallel search/stack hydration with project singleflight, topology-first progressive rendering, and privacy-redacted OTLP traces.
- Add user-scoped POSIX and Windows installers that register `glab mr-graph`, support latest or pinned upgrades, preserve unrelated aliases and existing binaries on failure, and provide matching safe uninstallers.

## [0.14.6] - 2026-08-23

### Fixed

- Back off automatic refreshes after failures instead of retrying every five minutes. Reported by [@syamichin](https://github.com/syamichin) in [#4](https://github.com/orangain/gh-pr-graph/issues/4) and fixed in [#6](https://github.com/orangain/gh-pr-graph/pull/6).
- Return empty arrays instead of `null` when no merge requests match a search. Reported by [@syamichin](https://github.com/syamichin) in the upstream [#3](https://github.com/orangain/gh-pr-graph/issues/3) and fixed in [#5](https://github.com/orangain/gh-pr-graph/pull/5).

## [0.14.5] - 2026-08-14

### Changed

- Unified the colors of the pending-review and re-review attention icons.

[Unreleased]: https://github.com/ota-takeru/glab-mr-graph/compare/v0.15.0...HEAD
[0.15.0]: https://github.com/ota-takeru/glab-mr-graph/compare/v0.14.6...v0.15.0
[0.14.6]: https://github.com/ota-takeru/glab-mr-graph/compare/v0.14.5...v0.14.6
[0.14.5]: https://github.com/ota-takeru/glab-mr-graph/compare/v0.14.4...v0.14.5
