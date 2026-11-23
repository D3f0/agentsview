# Build and Dependency Rules

Read this file before changing build commands, toolchain setup, CI build tags,
or frontend dependencies. Use the `Makefile` as the command reference.

## CI path filters

Update the CI path filters when adding a reader of files in another lane.

## Merge queue checks

Every PR runs the full CI suite before it can join the merge queue, and every
push to `main` runs it again. A merge group therefore runs only the checks that
two PRs, each green on its own, are likely to break together:

- `lint` (golangci-lint with the Huma API contract check, and NilAway)
- `frontend` (formatting, type checks, design-system checks, unit tests)
- `Docs and scripts`
- the Linux `test` suites: Sync, Other packages (including the SQLite schema and
  data-version tests in `internal/db` and `internal/parser`), and Eval ingest
- `e2e (chromium)`

Two PRs that bump a version to the same number make the same edit, so Git merges
them without a conflict. For the SQLite data version, the merge group catches
this: `TestParsedOutputMatchesDataVersion` fails when one version has two bump
records, and two `dataVersionScopes` entries for one version do not compile.

Merge groups skip `test-extended` (Windows, macOS, the x86-64-v2 startup check,
and the simulator), the isolated Windows suites, the race detector, the
PostgreSQL, CockroachDB, and ClickHouse integration suites, WebKit e2e, Node 26
frontend tests, desktop unit tests, and the released-command check. PostgreSQL
and ClickHouse schema changes therefore rely on the integration suites that run
on every PR and on `main`. A break that only a skipped suite catches surfaces in
`main` CI after the merge.

When you add a job, decide which group it belongs to. A job skips merge groups
when its `if` includes `github.event_name != 'merge_group'`. A matrix cannot
drop an `include` entry by event, so split queue entries into their own job (as
`test` and `test-extended` share steps through a YAML anchor), or list the
matrix values in an expression (as `e2e` does). Branch protection requires
`CI gate`, not individual CI jobs, so these changes need no ruleset update.

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
retained. Playwright jobs run browsers in Microsoft's public MCR image through a
remote server.

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
- Release builds must start on x86-64-v2 CPUs. CI builds the `fts5` binary and
  starts it under QEMU on a Nehalem CPU model (x86-64-v2), so a dependency
  that needs a newer CPU at startup fails CI. Check a new cgo or prebuilt
  native library against that baseline before adding it.
- Keep the Makefile's release-only link settings out of test and e2e builds.
- `DISTRIBUTION` identifies who ships the binary.
  Official direct downloads use `kenn` and keep self-updating, like builds
  with no value. Any other value names a package manager: those builds refuse
  `agentsview update` installs and point users at the package manager.
  `make homebrew` is the release build with `DISTRIBUTION=homebrew`, written
  to `$(BINDIR)/agentsview`.

## Kit checkers

`tools/go.mod` pins the kit checkers that CI and the hooks run: `kennlint` and
`huma-check`. Makefile targets run them with `go tool -modfile=tools/go.mod`, so
`tools/go.sum` verifies them.

Do not run kit tools as `go run go.kenn.io/kit/cmd/...@version`. That form also
fetches the newest kit `go.mod` to look for a deprecation notice and verifies it
against the checksum database. A kit release the checksum database has not
recorded yet then fails every such command.

`kennlint config -check` compares `.golangci.yml` with the config from the kit
version in `tools/go.mod`. That config must match the kit golangci plugin in
`.custom-gcl.yml`. Bump both together with
`go -C tools get -tool go.kenn.io/kit/cmd/kennlint@<version>` and
`go -C tools mod tidy`. Renovate skips `tools/go.mod` for this reason.

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

## Bob fork prerelease artifacts

The fork's `bob-macos-release.yml` workflow builds all six native platforms in
one dispatch: macOS, Linux and Windows on ARM64 and AMD64. Its single publish
job waits for every build, verifies all 24 files, uploads to a new draft and
checks remote hashes before publishing a prerelease. It never replaces a tag or
asset. Do not use upstream's release workflow, publish to PyPI, or ship desktop
installers for this fork release.

The supported new contract is `v0.48.0-bob.2.1`, package `0.48.0+bob.2.1`, API
29 and data version 186. Source ancestry must include upstream v0.48.0 commit
`14ab8dd347ec341f15ba0cf8ae519ee198d61557`. The exact source SHA is required at
dispatch; there is no default commit. Automation can run from the release branch
while the source checkout stays on the immutable tag. Both checkouts disable
persisted credentials.

1. Finish source review and verification in an isolated checkout. Prepare
   pricing and embedded assets, then run the relevant contract checks:

   ```bash
   npm ci --prefix frontend
   make pricing-snapshot ensure-embed-dir
   CGO_ENABLED=1 go test -tags fts5 ./internal/parser ./internal/config ./internal/sync -run Bob -count=1
   CGO_ENABLED=1 go test -tags fts5 ./internal/parser -run TestParsedOutputMatchesDataVersion -count=1
   python -m pytest scripts/bob_release_test.py
   ```

1. Record the final source commit as `SOURCE_SHA`. Publish only the new release
   branch and a new lightweight tag identifying that exact commit. Never move an
   existing tag, force-push, change upstream `main`, or select artifacts from
   another source. The workflow must already be visible on the fork's default
   branch for GitHub to accept a dispatch.

1. Dispatch from the release branch, not from a stale automation revision:

   ```bash
   gh workflow run bob-macos-release.yml --repo D3f0/agentsview \
     --ref release/v0.48.0-bob.2.1 \
     -f release_tag=v0.48.0-bob.2.1 -f source_sha="$SOURCE_SHA"
   ```

   Observe the resulting run with
   `gh run watch "$RUN_ID" --exit-status --repo D3f0/agentsview`. Only the
   successful six-platform run can publish. A failed upload leaves an
   unpublished draft; do not overwrite its assets or retry publication without
   investigating it. A new dispatch refuses an existing release, including a
   draft.

1. Download the published assets to a new scratch directory and independently
   verify the complete inventory:

   ```bash
   mkdir artifacts
   gh release download v0.48.0-bob.2.1 --repo D3f0/agentsview --dir artifacts
   python scripts/bob_release.py verify-all --tag v0.48.0-bob.2.1 \
     --source "$SOURCE_SHA" --directory artifacts
   ```

Each platform has a native archive, wheel, `BUILD` JSON and `SHA256SUMS`. Darwin
ARM64 retains `BUILD.json` and `SHA256SUMS`; the other platforms use
`BUILD-OS-ARCH.json` and `SHA256SUMS-OS-ARCH`. Native build jobs install the
wheel, execute both packaged binaries with scratch archives on loopback, and
check the full runtime version/source/API/data contract. The artifact gate
checks wheel version and platform metadata, binary equality with the native
archive and its recorded digest. Both binaries smoke-check PostgreSQL and
ClickHouse replica CLI commands; this does not replace backend database
integration tests. Bob parser/config/sync regressions and the parsed-output
data-version gate run before artifacts are uploaded.

The wheel launcher is checked with `--version`. Server smoke checks execute
the installed wheel's native binary directly, so the harness waits for the
process holding the scratch archive lock before removing that archive.

macOS binaries must target 13.0, match the native CPU and pass ad-hoc signing
verification; wheels use `macosx_13_0`. Linux uses digest-pinned manylinux 2.28
containers and verifies the highest required GLIBC symbol is at most 2.28.
Windows AMD64 uses checksummed UCRT GCC 15; ARM64 uses checksummed native
llvm-mingw. Do not silently substitute compilers or newer platform baselines. Go
follows the source `go.mod`; Node is pinned by the workflow. Builds omit the
telemetry ingest key and use `DISTRIBUTION=d3f0`.

Release notes include the exact source, all six `uvx --from` wheel URLs, Bob 1.x
CLI/IDE and Bob 2.x SQLite compatibility, and the archive-copy upgrade limit.
Keep the original archive backup untouched. Scope 186 reparses Bob, Antigravity,
Antigravity CLI and DeepSeek Harness after the unchanged upstream 168/169
scopes; older binaries cannot open an upgraded archive. PostgreSQL and
ClickHouse remain supported; DuckDB was removed upstream.

The same script can verify or mirror the legacy `v0.47.0-bob.2.2` release with
its original source `8823ce27240abacf3ddac4214db70a266941f82e`, API 15 and data
version 169. This is an explicit historical contract, not a metadata downgrade.
Mirroring requires all six verified platforms and an already existing
exact-source tag at the destination:

```bash
python scripts/bob_release.py mirror --tag "$TAG" --source "$SOURCE_SHA" \
  --directory artifacts --source-repo D3f0/agentsview \
  --target-repo "$TARGET_REPO"
```

Mirroring supports a separate GitHub or Enterprise repository, rewrites only
release-download URLs and resumes only an identical unpublished draft. It
verifies remote digests, downloading bytes when digests are unavailable, and
refuses mismatched metadata, extra assets or overwrites. It never pushes
branches or creates tags. New release publication always requires all six
platforms; `append` is reserved for the historical release contract.
