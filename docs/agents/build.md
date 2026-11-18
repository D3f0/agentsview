# Build and Dependency Rules

Read this file before changing build commands, toolchain setup, CI build tags,
or frontend dependencies. Use the `Makefile` as the command reference.

## CI path filters

Update the CI path filters when adding a reader of files in another lane.

## CI runner policy

CI jobs use standard GitHub-hosted runners, including fork pull requests,
scheduled checks, and desktop builds. Select labels from
[GitHub's supported runners](https://docs.github.com/en/actions/reference/runners/github-hosted-runners).
Keep the explicit architecture labels for Linux ARM64 and macOS desktop builds.

The exceptions are the seconds-long `CI gate`, `changes`, and
`Sibling pins on main` jobs. They run on the Namespace profile
`namespace-profile-linux-1cpu` so they do not wait in the GitHub-hosted queue.

Use the local `go-cache-restore` and `go-cache-save` actions for Go caches and
disable the built-in cache in `actions/setup-go`. Use npm caching in
`actions/setup-node`. Set `cache-dependency-path` to the lockfiles for the
packages the job installs. Desktop jobs cache Rust dependencies through
`Swatinem/rust-cache` with the GitHub cache backend. Pull-request workflow
callers set `cache-mode: read` so jobs can restore caches without saving copies.
Cache owners opt in with `owner: true` to keep build entries used during the run
and outputs referenced by fresh action records when refreshing a daily cache.
Module caches stay on their existing restore and save paths, with all modules
retained. Playwright jobs run browsers in Microsoft's public MCR image through
a remote server.

`desktop-artifacts.yml` builds the frontend once on Linux and packages those
assets in its Windows, Linux, and Linux ARM64 desktop builds.

Go builds and tests use `-trimpath` everywhere: the `Makefile` exports it in
`GOFLAGS`, the Go workflows set `GOFLAGS: -trimpath`, and desktop sidecar builds
pass it unless `AGENTSVIEW_SIDECAR_TRIMPATH=false`. Matching flags let desktop
builds reuse compile results from test jobs and let local worktrees share one
build cache. See the [README](../../README.md#shared-go-build-cache) for opting
out and debugging.

Windows Go test and lint jobs in `ci.yml` put caches and temp files under
`RUNNER_TEMP`. Capture tests use a private system-drive temporary directory so
ancestor permissions satisfy their checks.

## Go and SQLite

- Build with `CGO_ENABLED=1`; the SQLite driver requires CGO.
- Use the `fts5` build tag for full-text search.
- The Makefile's `LDFLAGS` set the PostHog project key with
  `-X go.kenn.io/kit/telemetry/posthog.projectKey`. Builds without it send no
  telemetry, so keep it out of test and e2e builds.
- `DISTRIBUTION` sets `main.distribution` and
  `go.kenn.io/kit/telemetry/posthog.distribution` to who ships the binary.
  Official direct downloads use `kenn` and keep self-updating, like builds
  with no value. Any other value names a package manager: those builds refuse
  `agentsview update` installs and point users at the package manager.
  `make homebrew` is the release build with `DISTRIBUTION=homebrew`, written
  to `$(BINDIR)/agentsview`.

## Frontend

- The embedded Svelte frontend requires Node.js and the frontend toolchain. Read
  `frontend/AGENTS.md` before working in that directory.
- `@kenn-io/kit-ui` is a public git dependency pinned to a commit in
  `frontend/package.json`.
- The lockfile records the GitHub dependency as an SSH URL because npm uses that
  canonical form. npm still fetches it anonymously over HTTPS. Do not rewrite
  the lockfile URL.
- To update kit-ui, change the commit hash in `frontend/package.json` and run
  `npm install`.

## Bob release distribution

The Bob prerelease `v0.47.0-bob.2.2` ships application source
`8823ce27240abacf3ddac4214db70a266941f82e` as package version
`0.47.0+bob.2.2`. Release automation is checked out separately from the exact
tagged application source; changing automation must not change that source.

`.github/workflows/bob-macos-release.yml` builds native binaries and wheels for
Darwin, Linux, and Windows, each on AMD64 and ARM64. Maintainers select one OS
and architecture per manual dispatch and specify the existing Bob branch with
`--ref upstream-v0.46.1-bob`. Pushes do not publish artifacts. Dispatch only
missing platforms; a repeated platform fails rather than replacing assets.

Linux uses pinned manylinux 2.28 containers on matching native runners and
checks the binary's glibc requirements. macOS requires 13.0 and ad hoc signing.
Windows uses MinGW GCC on AMD64 and checksum-pinned llvm-mingw on ARM64, with
the SQLite header from the exact bundled amalgamation. Windows archives are
ZIP files containing `agentsview.exe`; other archives are gzip-compressed tar
files. Every platform installs its wheel and checks native execution, server
API/data versions, replica CLI contracts, and Bob parser/config/sync tests.

One publication job appends verified platform inventories sequentially. Runs
for the same tag are serialized, and existing assets are never overwritten.
Darwin ARM64 alone creates the initial prerelease; all other platforms use
`bob_release.py append` against that existing release. Platform-specific build
and checksum manifests preserve the initial `BUILD.json` and `SHA256SUMS`.
Distribute archives and wheels through GitHub release assets only: no desktop
installers and no PyPI publication.

To mirror the complete release, first push its exact source tag to the target
repository without moving a branch. Download all release assets into one clean
directory, then run:

```bash
python scripts/bob_release.py mirror \
  --tag v0.47.0-bob.2.2 \
  --source 8823ce27240abacf3ddac4214db70a266941f82e \
  --directory release-assets \
  --source-repo D3f0/agentsview \
  --target-repo HOST/OWNER/REPO
```

The mirror requires all six platforms and checks the public release against
local checksums. It creates a target draft or resumes an identical draft,
uploads only missing assets, verifies the uploaded bytes, and publishes a
prerelease. An existing published target release is never replaced. Enterprise
servers without asset digests are verified by downloading and hashing each
asset before publication.
