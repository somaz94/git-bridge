# Changelog

All notable changes to this project will be documented in this file.

## Unreleased (2026-07-20)

### Bug Fixes

- warn when bump-version.sh finds no version to replace ([9a895d9](https://github.com/somaz94/git-bridge/commit/9a895d91afd1dcc37b50f30538402363d48db07d))

### Builds

- **deps:** Bump actions/setup-go from 6 to 7 (#19) ([#19](https://github.com/somaz94/git-bridge/pull/19)) ([3b1d059](https://github.com/somaz94/git-bridge/commit/3b1d0596af9e47b5aa1099ae64de18c10bcd0c90))
- **deps:** Bump the go-minor group with 3 updates (#20) ([#20](https://github.com/somaz94/git-bridge/pull/20)) ([f1b745b](https://github.com/somaz94/git-bridge/commit/f1b745baf77711c5b002b3627cbe6acb093aecda))
- **deps:** Bump the go-minor group with 3 updates (#18) ([#18](https://github.com/somaz94/git-bridge/pull/18)) ([9a4e934](https://github.com/somaz94/git-bridge/commit/9a4e934c51473586009a71de7573180b2d6d10d1))
- **deps:** Bump the go-minor group with 4 updates (#17) ([#17](https://github.com/somaz94/git-bridge/pull/17)) ([caed80a](https://github.com/somaz94/git-bridge/commit/caed80a9e437e12ea2c5a0c512f5397d44f5ecba))
- **deps:** Bump actions/checkout from 6 to 7 (#16) ([#16](https://github.com/somaz94/git-bridge/pull/16)) ([c0ab26b](https://github.com/somaz94/git-bridge/commit/c0ab26b7a150d9384d35db71addcee92c867d6d2))
- **deps:** Bump the go-minor group with 4 updates (#15) ([#15](https://github.com/somaz94/git-bridge/pull/15)) ([f5f0688](https://github.com/somaz94/git-bridge/commit/f5f06885cbf95a14805df8049a4a4658524f890c))
- **deps:** Bump alpine from 3.23 to 3.24 in the docker-minor group (#14) ([#14](https://github.com/somaz94/git-bridge/pull/14)) ([ed1ba63](https://github.com/somaz94/git-bridge/commit/ed1ba636ed1c2e98ac8e8adad48e8dd0512af630))
- **deps:** Bump the go-minor group with 4 updates (#13) ([#13](https://github.com/somaz94/git-bridge/pull/13)) ([950da4c](https://github.com/somaz94/git-bridge/commit/950da4c8e79f06ece258ef69221f0db497c48d52))

### Continuous Integration

- remove DCO workflow ([f9a11aa](https://github.com/somaz94/git-bridge/commit/f9a11aab4d2a7cb34683ac603efdeeac00ce664d))
- adopt semantic-pr, labels, lock-threads, PR size, and auto-assign reusables ([87d0b7d](https://github.com/somaz94/git-bridge/commit/87d0b7d418f025d9af346ffd4eba89c7bb05883d))
- use reusable stale-issues workflow ([1005aa3](https://github.com/somaz94/git-bridge/commit/1005aa31c6bd1067d5b1eb1d0a364a0b0c9e082c))
- use reusable issue-greeting workflow ([eb2a54c](https://github.com/somaz94/git-bridge/commit/eb2a54c574a4c9142c8a93e18b199ba987010124))
- use reusable dependabot-auto-merge workflow ([bdb5bf2](https://github.com/somaz94/git-bridge/commit/bdb5bf2288f607f37bf064d2ac9ff77dc1dd4c11))
- use reusable contributors workflow ([f35acb9](https://github.com/somaz94/git-bridge/commit/f35acb961d8e7bb702c915e5a6f7d73682355420))
- add ok-to-test workflow stub ([4f5077e](https://github.com/somaz94/git-bridge/commit/4f5077ea2a82f2f11834b071a05a9daf55e55702))
- add PR welcome workflow stub ([b43c8b0](https://github.com/somaz94/git-bridge/commit/b43c8b04ea0c4cde6b417ab5a3ef5a820e92c3e2))
- add DCO check via shared reusable workflow ([d4b0ef4](https://github.com/somaz94/git-bridge/commit/d4b0ef4bcb984f91b384a57622583abab253b17e))

### Contributors

- somaz

<br/>

## [v0.6.0](https://github.com/somaz94/git-bridge/compare/v0.5.0...v0.6.0) (2026-06-01)

### Features

- add ref_overrides and bidirectional delete propagation ([bd5f5cc](https://github.com/somaz94/git-bridge/commit/bd5f5cc9b1c6c898300eddba4f228b1a37890287))

### Builds

- **deps:** Bump the go-minor group with 4 updates (#12) ([#12](https://github.com/somaz94/git-bridge/pull/12)) ([732dc39](https://github.com/somaz94/git-bridge/commit/732dc39da4f120fd8313692c2444f9af16a1b5b3))

### Continuous Integration

- add concurrency guards to recurring workflows ([e48c3d5](https://github.com/somaz94/git-bridge/commit/e48c3d556177134139a36c0b85018144b2c61afa))

### Contributors

- somaz

<br/>

## [v0.5.0](https://github.com/somaz94/git-bridge/compare/v0.4.0...v0.5.0) (2026-05-26)

### Features

- add /retry/mirror API + per-repo Slack routing + retry_direction (v0.5.0) ([75757e9](https://github.com/somaz94/git-bridge/commit/75757e9bd65ca2d4d70d0613decbbb2d52a89b67))
- **ci:** publish Helm chart to GHCR (OCI) alongside gh-pages ([325827d](https://github.com/somaz94/git-bridge/commit/325827d7f14781a5d08fa6e0e885ac6704ee6315))

### Bug Fixes

- **ci:** use staged tarball for OCI push (gh-pages branch checkout invalidates ./helm/ path) ([ace943b](https://github.com/somaz94/git-bridge/commit/ace943b00b51f1f4ebd8d2b76a46348a19ae463e))

### Builds

- **deps:** Bump the go-minor group with 4 updates (#11) ([#11](https://github.com/somaz94/git-bridge/pull/11)) ([2ec917c](https://github.com/somaz94/git-bridge/commit/2ec917cf7089b8566478c9b3bdd30351f52adbea))
- **deps:** Bump the go-minor group with 4 updates (#10) ([#10](https://github.com/somaz94/git-bridge/pull/10)) ([e84f5b7](https://github.com/somaz94/git-bridge/commit/e84f5b7046dccefe8f5ace255a430b61b2b96100))
- **deps:** Bump github.com/aws/aws-sdk-go-v2/config (#9) ([#9](https://github.com/somaz94/git-bridge/pull/9)) ([a618c6c](https://github.com/somaz94/git-bridge/commit/a618c6cc9b02b23512eff9c605a29c89bda2d03b))
- **deps:** Bump docker/login-action from 3 to 4 ([98fd916](https://github.com/somaz94/git-bridge/commit/98fd9166b57e1535a932d44a2613c313a2648213))
- **deps:** Bump docker/setup-qemu-action from 3 to 4 ([487e92a](https://github.com/somaz94/git-bridge/commit/487e92a3b8c41fa594e9443d3413091587b75d14))

### Continuous Integration

- use helm-chart-release-action@v1 (replace inline release script) ([86351e2](https://github.com/somaz94/git-bridge/commit/86351e2b7a0b5936b7e427f271ea7804f282ca2b))

### Contributors

- somaz

<br/>

## [v0.4.0](https://github.com/somaz94/git-bridge/compare/v0.3.0...v0.4.0) (2026-04-14)

### Features

- **helm:** add helm chart for git-bridge ([4d92f5d](https://github.com/somaz94/git-bridge/commit/4d92f5d91a9c0f4262e6f1fd14a10cbddcc4c346))
- **version:** add version package and -version flag ([7cd0aa9](https://github.com/somaz94/git-bridge/commit/7cd0aa95ade7dd44ca2a11b1079f0c77714699e7))

### Documentation

- add version guide and refresh development docs ([bbd95d9](https://github.com/somaz94/git-bridge/commit/bbd95d9d3c5eaa9d5552952119bf398da5b9329c))
- remove duplicate rules covered by global CLAUDE.md ([aaf3062](https://github.com/somaz94/git-bridge/commit/aaf306208cd7400fdc4215817f43a2652223be24))
- add DEVELOPMENT.md ([940ac4d](https://github.com/somaz94/git-bridge/commit/940ac4d458fd655efa4c77a683eee52adeb0a2a5))

### Builds

- **deps:** Bump actions/github-script from 8 to 9 ([6150862](https://github.com/somaz94/git-bridge/commit/61508623a9d9fa731aada4d34192891f56881291))
- **deps:** Bump dependabot/fetch-metadata from 2 to 3 ([4d84825](https://github.com/somaz94/git-bridge/commit/4d84825005c7b99d160b9503c913fab3b47e6a88))
- **deps:** Bump the go-minor group with 2 updates (#4) ([#4](https://github.com/somaz94/git-bridge/pull/4)) ([338539f](https://github.com/somaz94/git-bridge/commit/338539f8a68e99bb2f166c3e6b98f63f9664f59e))
- **deps:** Bump the go-minor group with 4 updates (#3) ([#3](https://github.com/somaz94/git-bridge/pull/3)) ([2d09da5](https://github.com/somaz94/git-bridge/commit/2d09da5ca3785304d48d6165174f9933acf319db))

### Continuous Integration

- add lint and helm-release workflows with version build-args ([297efa0](https://github.com/somaz94/git-bridge/commit/297efa09badcdbc78774f614dd808dabca9dd59b))
- restructure Makefile, Dockerfile, and hack scripts ([e157c39](https://github.com/somaz94/git-bridge/commit/e157c3909021ff266f63d7c11cd694b47353fed4))
- add Docker build and push job to release workflow ([9c35281](https://github.com/somaz94/git-bridge/commit/9c352818b53e73c9518cd36858653b590bb90446))
- skip auto-generated changelog and contributors commits in release notes ([2596fd7](https://github.com/somaz94/git-bridge/commit/2596fd772b9524818192e495810e566188cac397))
- revert to body_path RELEASE.md in release workflow ([d81317d](https://github.com/somaz94/git-bridge/commit/d81317dcb8c7893b569a521900338e85d7937b5b))
- use generate_release_notes instead of RELEASE.md ([d729059](https://github.com/somaz94/git-bridge/commit/d729059857512f9a05199643198af8525fe97049))
- add auto-generated PR body script for make pr ([8c4ed4d](https://github.com/somaz94/git-bridge/commit/8c4ed4dae184189c14f5f1c04833778e93c212b4))

### Chores

- bump version to v0.4.0 ([846c48e](https://github.com/somaz94/git-bridge/commit/846c48ec8b6fef7be0ad985a8295cc9bead3dbb1))
- remove duplicate rules from CLAUDE.md (moved to global) ([f7c0b51](https://github.com/somaz94/git-bridge/commit/f7c0b517d6dcd0a17b9d387c5e6c42404e8c52f9))
- add git config protection to CLAUDE.md ([ea02f04](https://github.com/somaz94/git-bridge/commit/ea02f044456e2e392f8305da15df2cf450c55b46))
- add workflow Makefile targets (check-gh, branch, pr) ([7239588](https://github.com/somaz94/git-bridge/commit/72395889b55d652473e8c3761117b8cd014e7f6c))

### Contributors

- somaz

<br/>

## [v0.3.0](https://github.com/somaz94/git-bridge/compare/v0.2.0...v0.3.0) (2026-03-20)

### Features

- sync with internal repo - commit author, porcelain push, config validation ([5b0d904](https://github.com/somaz94/git-bridge/commit/5b0d904f996df8121e82da7a696fba4b64f0df76))
- add CODEOWNERS ([1ad31e0](https://github.com/somaz94/git-bridge/commit/1ad31e068d0d3d01847db9511155ebfb0b84bdc4))

### Bug Fixes

- use GITHUB_TOKEN for dependabot auto merge ([83c06dd](https://github.com/somaz94/git-bridge/commit/83c06dd83b383d48046d68a43d31c0e19a7cb87e))

### Documentation

- add no-push rule to CLAUDE.md ([6e640ae](https://github.com/somaz94/git-bridge/commit/6e640ae398829a344eda10b8c2ea7aab8f73f872))

### Builds

- **deps:** Bump the go-minor group with 4 updates (#2) ([#2](https://github.com/somaz94/git-bridge/pull/2)) ([7fda2df](https://github.com/somaz94/git-bridge/commit/7fda2dfe2ac3db9454095159089d08445229f297))

### Continuous Integration

- migrate gitlab-mirror workflow to multi-git-mirror action ([3ab4ce3](https://github.com/somaz94/git-bridge/commit/3ab4ce3f46d1ecf375d8dbd70179794270a3cdb5))

### Contributors

- somaz

<br/>

## [v0.2.0](https://github.com/somaz94/git-bridge/compare/v0.1.0...v0.2.0) (2026-03-18)

### Features

- add committer info (Pushed by) to Slack mirror sync notifications ([36acacd](https://github.com/somaz94/git-bridge/commit/36acacdf72dc83e6b60acb147a05856fbefc9b96))
- implement incremental fetch with PVC-backed mirror cache ([5c402e5](https://github.com/somaz94/git-bridge/commit/5c402e59ac39cc4ee8f380b6b798fd9df25c32b8))

### Documentation

- CLAUDE.md ([0acbff3](https://github.com/somaz94/git-bridge/commit/0acbff35a74d5ac485572df8a9e21a44102b5bb9))
- add CLAUDE.md project guide ([1afd0ef](https://github.com/somaz94/git-bridge/commit/1afd0ef3f3a10db20cbb55fa406103530eb8748c))
- README.md ([2064c8a](https://github.com/somaz94/git-bridge/commit/2064c8ad404f2b3029a07652eecb44cdc6ca7aa9))

### Tests

- improve coverage from 93% to 97.9% and separate make test/test-cover roles ([0f65504](https://github.com/somaz94/git-bridge/commit/0f65504f047a4132e9312095aa64fd49b788ed5c))

### Continuous Integration

- use somaz94/contributors-action@v1 for contributors generation ([49fd3a5](https://github.com/somaz94/git-bridge/commit/49fd3a56852728eb8b5eb35ea6954d156e916803))
- use major-tag-action for version tag updates ([11b9d93](https://github.com/somaz94/git-bridge/commit/11b9d9356498ab84e53301ce1ddccb0ea81504cf))
- migrate changelog generator to go-changelog-action ([6510563](https://github.com/somaz94/git-bridge/commit/65105638df73f3ea8139b396c40470e07fc8efe3))
- add GitHub release notes configuration ([4fbc5d9](https://github.com/somaz94/git-bridge/commit/4fbc5d95d0693f94680bf77e4a39b5485f9c5eff))
- unify changelog-generator with flexible tag pattern ([a8778f6](https://github.com/somaz94/git-bridge/commit/a8778f6ceed28908975c22cea9fb8b285ccd5574))

### Contributors

- somaz

<br/>

## [v0.1.0](https://github.com/somaz94/git-bridge/compare/v0.0.1...v0.1.0) (2026-03-13)

### Features

- add DockerHub multi-arch build and push support ([2c0aca7](https://github.com/somaz94/git-bridge/commit/2c0aca7c709ce510aa4a0000dcba1ab85c612218))
- add K8s manifests and example configurations ([b25c610](https://github.com/somaz94/git-bridge/commit/b25c610480486088e0ce77d9cb1a96a2144784b4))
- add core mirroring engine with multi-provider support ([f70823e](https://github.com/somaz94/git-bridge/commit/f70823ef46b6fd4712815e93e97a8f05d5f1d912))

### Bug Fixes

- skip major version tag deletion on first release ([cbadec1](https://github.com/somaz94/git-bridge/commit/cbadec148ae7e35b1560c74cca85b6721ce7fd5c))
- remove docker job from release workflow ([580e593](https://github.com/somaz94/git-bridge/commit/580e593305e20a4c1af308c007343d3a5064a1c3))
- fix changelog-generator tag handling and dependabot secrets access ([553a875](https://github.com/somaz94/git-bridge/commit/553a875849cd975aa45e74986c78f77ce58e3166))

### Documentation

- add documentation, architecture diagram, and update README ([c4d3418](https://github.com/somaz94/git-bridge/commit/c4d341832f629610a0fd4760a5870cf24d751432))
- add documentation, architecture diagram, and update README ([3154eae](https://github.com/somaz94/git-bridge/commit/3154eae3430e8ff0d810e3fb2b7b6d3db4033630))

### Builds

- **deps:** Bump alpine from 3.21 to 3.23 in the docker-minor group ([cb2d032](https://github.com/somaz94/git-bridge/commit/cb2d03215e8b9b0dac690817fb5b4a4b63700e8f))
- **deps:** Bump alpine from 3.21 to 3.23 in the docker-minor group ([1e387db](https://github.com/somaz94/git-bridge/commit/1e387dba39f0c50de032915e88a0ed2d1189f123))

### Continuous Integration

- add GitHub Actions workflows and dependabot config ([a73d969](https://github.com/somaz94/git-bridge/commit/a73d9699f6f14bd08d26b2f8d8a0c7be30785df0))

### Contributors

- somaz

<br/>

## [v0.0.1](https://github.com/somaz94/git-bridge/releases/tag/v0.0.1) (2026-03-13)

### Contributors

- somaz

<br/>

