# Releasing Epitrop

A release is a `v*` tag on a commit of `main`. The tag starts `.github/workflows/release.yml`, which publishes `epitropctl`, the snapshot schema and the evidence that they were built from that commit. The first tag is an operator decision.

The flow follows Praetor's: the same GoReleaser layout, `changelog.d/` fragments rendered by `praetorctl release`, and a tag made after the merge ([Praetor's releasing guide](https://github.com/cordanaLLM/praetor/blob/main/docs/guides/releasing.md)).

## What a tag publishes

`release.yml` calls the reusable `.github/workflows/release-binaries.yml`. That workflow runs `goreleaser check`, takes the release notes from the changelog, builds `./cmd/epitropctl` into a draft release (`.goreleaser.yaml`), signs `checksums.txt` with keyless cosign, verifies that signature against its own workflow identity, attests every checksummed artefact with `actions/attest-build-provenance`, and only then publishes the release. `praetorctl audit` measures this as SLSA Build Level 3 with cosign signing and an SBOM, so `.standards.yaml` declares no HISS-11 exception.

| Asset | Content |
| --- | --- |
| `epitropctl_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) | the binary for linux, darwin and windows on amd64 and arm64, with `README.md`, the licences and `NOTICE.md` (third-party notices) |
| `<archive>.cyclonedx.json` | a CycloneDX SBOM for each archive (syft) |
| `checksums.txt`, `checksums.txt.sigstore.json` | SHA-256 of every archive, every SBOM and the snapshot schema, and the keyless cosign bundle of that list |
| `snapshot.schema.json` | the snapshot contract this release writes (`catalog/snapshot.schema.json`, `schema_version` 1) |

The signature covers `checksums.txt`, and the build provenance names every file that `checksums.txt` lists. A file is covered by both only when it is listed there. The schema is uploaded by `release.extra_files` and listed by `checksum.extra_files` in `.goreleaser.yaml`; the two entries must name the same file. Up to v0.1.0-rc.2 only the first entry existed, so that release's schema carries neither a signature nor provenance.

The release notes are the tag's section of `CHANGELOG.md`. `tools/releasenotes` writes that section to a file, and GoReleaser takes it with `--release-notes` in place of a list of commit subjects. A tag whose version has no section, two sections or an empty one stops the run before anything is built (`go test ./tools/releasenotes`).

`epitropctl version` prints the version the release build injected (`-X main.version`); a build from source prints `dev`.

## The dry run

`.github/workflows/release-dry-run.yml` builds the same archives and SBOMs on pull requests and on `main`, with the tool versions of `release-binaries.yml`: `goreleaser check`, then `goreleaser release --snapshot --clean --skip=sign`. It signs nothing and publishes nothing. It then checks that the snapshot holds six archives and six SBOMs, that `checksums.txt` lists exactly those twelve files and the snapshot schema with matching digests, and that the linux/amd64 binary reports a version ending in `-snapshot`. A snapshot builds no release notes, so the dry run also runs `tools/releasenotes` on the newest released section of the changelog. It runs when Go sources, `go.mod`, `.goreleaser.yaml`, `CHANGELOG.md`, the files packed into the archives or a release workflow change, so a broken release build shows on the change that broke it and not on the day of a release. It is not a required check.

## Verifying a release

```bash
cosign verify-blob \
  --certificate-identity "https://github.com/tekmora/epitrop/.github/workflows/release-binaries.yml@refs/tags/<tag>" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
sha256sum --check --ignore-missing checksums.txt
gh attestation verify epitropctl_<version>_linux_amd64.tar.gz --repo tekmora/epitrop
gh attestation verify snapshot.schema.json --repo tekmora/epitrop
```

`gh attestation verify` needs a logged-in `gh` (2.102.0 refuses without one). Without an account, fetch the provenance bundle and verify it with cosign. One bundle names every file in `checksums.txt`, so the bundle of one file verifies the others too:

```bash
digest=$(sha256sum snapshot.schema.json | cut -d' ' -f1)
curl -fsS "https://api.github.com/repos/tekmora/epitrop/attestations/sha256:$digest" \
  | jq -c '.attestations[0].bundle' > provenance.json
cosign verify-blob-attestation --new-bundle-format --bundle provenance.json --type slsaprovenance1 \
  --certificate-identity "https://github.com/tekmora/epitrop/.github/workflows/release-binaries.yml@refs/tags/<tag>" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  snapshot.schema.json
```

## How Praetor pins Epitrop

Praetor reads snapshots through the catalog package, pinned as a Go module: `github.com/tekmora/epitrop/catalog` at a pseudo-version before the first tag, at a tag after it. The package imports only the standard library (`TestCatalogImportsStdlibOnly`) and embeds the schema as `catalog.SnapshotSchema()`. The binary comes from the release assets.

The fixtures in `catalog/testdata/contract/` state the snapshot contract for a consumer: `cases.json` lists each case's expected outcome, and generated cases duplicate the first record of a source snapshot with suffixed `model_id` values instead of writing new files. The Go module archive carries the folder, so a consumer reads it from its module cache at the pinned version (`go list -m -f '{{.Dir}}' github.com/tekmora/epitrop`); `TestSnapshotContractFixtures` replays them here.

## Cutting a release

1. Every merged user-visible change carries one fragment under `changelog.d/`: `type`, `title`, optional `issue`, and `breaking`.
2. On a branch cut from `origin/main`, render the changelog. This rewrites `CHANGELOG.md` and removes the rendered fragments:

   ```bash
   praetorctl release --version=<tag> --date=<YYYY-MM-DD>
   ```

3. Bump the `README.md` release badge `message=v<version>` in the same branch; `TestReadmeReleaseBadgeMatchesChangelog` in `tools/releasenotes` fails until it matches the newest released section in `CHANGELOG.md`.
4. Merge that change through a pull request. The section it adds to `CHANGELOG.md` becomes the release notes.
5. Tag the merged commit and push only the tag:

   ```bash
   git fetch origin
   git tag -s <tag> -m "<tag>" origin/main
   git push origin <tag>
   ```

6. Check the run with `gh run list --workflow release.yml --limit 1` and the result with `gh release view <tag>`.

## Upgrade fixtures

A new build must read what earlier releases wrote. `testdata/v0.1.0/` holds the state the published v0.1.0 binary wrote: an event log with unit and graph records, a snapshot, a release watch seen-set, and one finished run of a supervised job (its `job.started` and `job.exited` records and the shim's job record `jobs/fixture-job.json`, in an event log of its own). `testdata/v0.1.0/generate.sh` is the command that wrote them; `bash generate.sh jobs` rewrites one part and leaves the others as they are.

The tests copy the set and never change it in place:

- `TestSystemUpgrade_*` in `cmd/epitropctl/system_upgrade_test.go` run the current binary against the copy: show, resume and append to the event log, run the release watch, and report and restart the job.
- `TestReadJobRecordReadsTheV010Record` and `TestJobReducerReplaysTheV010Events` in `internal/supervisor/upgrade_test.go` check the job record and the job events field by field. The shim writes and reads the record through one struct, so a renamed field passes every other test.
