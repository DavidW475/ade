# Contributing to ADE

Thanks for your interest! The plan is in [`doc/`](doc/README.md); how to use ADE is described in
the [user guide](doc/guide.md).

## Requirements

- Go (version as in `go.mod`, the current stable release is recommended)
- Node.js 22+ and npm
- [golangci-lint](https://golangci-lint.run/) v2 for `make lint`
- Development preferably in WSL2 or Linux, with the repository on the Linux file system (not
  under `/mnt/c`)

## Building and testing

```sh
make build   # build the frontend (web/ → internal/api/dist) and produce bin/ade
make test    # Go tests with the race detector + TypeScript type check
make lint    # golangci-lint and ESLint
```

`go build` and `go test` also work without a frontend build; the dashboard then only shows a
notice.

Tests against a real Docker daemon (sandbox) only run with `ADE_TEST_DOCKER=1`; they build a
small Debian image with git (`ade-test-git:1`) or use `ADE_TEST_IMAGE`:

```sh
ADE_TEST_DOCKER=1 go test ./internal/sandbox/ ./internal/agent/
make image   # build the sandbox image ade-agent-base locally (tag :dev)
```

## Developing locally

```sh
make dev                # daemon in the foreground (go run, debug logging)
cd web && npm run dev   # Vite dev server on http://localhost:5173 (forwards /api to :7000)
bin/ade start --no-browser   # prints the login URL
```

The login URL sets the session cookie for `localhost`; after that the Vite dev server works too.

An own configuration (different port, own data directory) keeps development separate from an
installed ADE:

```sh
cat > /tmp/ade-dev.yaml <<'YAML'
listen: 127.0.0.1:7099
data_dir: /tmp/ade-dev/data
workspaces_dir: /tmp/ade-dev/workspaces
YAML
go run ./cmd/ade serve --config /tmp/ade-dev.yaml --debug
```

Writing a new adapter for another agent CLI is described in [doc/adapters.md](doc/adapters.md).

## Issues and pull requests

The workflow (a branch per milestone, `Closes #N` per completed issue in the PR, checking after
the merge) is described in [AGENTS.md](AGENTS.md#workflow-milestones-issues-and-prs) and applies
to humans just the same.

## Releases

Releases are made by the maintainer, through `CHANGELOG.md` (ADR-024): if, after a merge to
`main`, the top of the file has a version `## [X.Y.Z] – YYYY-MM-DD` (or `## [X.Y.Z-rc.N] – …`
for a pre-release) that has no tag yet,
[`auto-release.yml`](.github/workflows/auto-release.yml) sets the tag `vX.Y.Z` on that commit and
starts [`release.yml`](.github/workflows/release.yml):

- **GoReleaser** builds `ade_linux_amd64.tar.gz` and `ade_linux_arm64.tar.gz` (version `vX.Y.Z`
  in the binary), `checksums.txt` and an SPDX SBOM per archive, uploads `install.sh` as well and
  uses the matching section of `CHANGELOG.md` as release notes. Every file in `checksums.txt`
  gets a build provenance attestation.
- **Images** (`images.yml`, in parallel): `ghcr.io/davidw475/ade-agent-base` and `ade-dev` with
  the tag `X.Y.Z` (without `v`; for full releases also `X.Y`), SBOM and provenance. By default the
  binary uses exactly `ade-agent-base:X.Y.Z` (`sandbox.DefaultImage`), which is why the images are
  built for every release, even without changes under `images/`.

The version is read by [`scripts/changelog-version.sh`](scripts/changelog-version.sh). It checks
format, date and order; the same check runs in every PR (job "Shell scripts").

Steps:

1. In `CHANGELOG.md`, rename the section `[Unreleased]` to `## [X.Y.Z] – YYYY-MM-DD`, add a new
   empty `[Unreleased]` and extend the comparison links at the end; open a PR to `main`.
   **The merge publishes.** Check locally: `scripts/changelog-version.sh` prints the new version.
2. Optionally beforehand (needs [GoReleaser](https://goreleaser.com/install/)): `goreleaser check`
   and `goreleaser release --snapshot --clean --skip=sbom`. In PRs that change the release
   configuration, the same snapshot including `install.sh` runs in CI
   ([`release-snapshot.yml`](.github/workflows/release-snapshot.yml)).
3. After the merge, follow the run: `gh run watch` (workflow "Auto-Release"). Then check:
   ```sh
   gh release view vX.Y.Z
   gh release download vX.Y.Z -p 'ade_linux_amd64.tar.gz' -D /tmp/ade-rel
   gh attestation verify /tmp/ade-rel/ade_linux_amd64.tar.gz --repo DavidW475/ade
   gh attestation verify oci://ghcr.io/davidw475/ade-agent-base:X.Y.Z --repo DavidW475/ade
   ADE_VERSION=vX.Y.Z sh install.sh && ade version
   ```
   If only the image build fails, the release is already published: fix the error and repeat the
   run with "Re-run failed jobs". If the run fails before the release exists, fix the error and
   start "Auto-Release" manually (`gh workflow run auto-release.yml`): if the tag exists but the
   release does not, the workflow catches up.

A manually pushed tag (`git tag -a vX.Y.Z -m "ADE vX.Y.Z" && git push origin vX.Y.Z`) still
starts `release.yml` directly; the CHANGELOG section must exist anyway.

## Conventions

- **Language:** English for docs, comments, error messages, log and UI texts, and identifiers.
- **Commits:** [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:` …).
- **Configuration:** Add new or changed YAML fields to [`doc/configuration.md`](doc/configuration.md);
  `internal/config/reference_test.go` checks the tables against the Go structs.
- **Architecture decisions** are appended as an ADR to [`doc/decisions.md`](doc/decisions.md).
- **Security:** The daemon can execute code. Changes to auth, the host/origin check, sandbox or
  proxy need tests.
- New dependencies sparingly and only with an Apache-2.0-compatible license.

## License

Contributions are licensed under the [Apache-2.0 license](LICENSE).
