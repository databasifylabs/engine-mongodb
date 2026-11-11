---
name: adding-a-version-line
description: "Add a new MongoDB version line to this engine repository: the versions.json entry and its pinned tools, any per-line differences in the engine code, and the local build, smoke, conformance and capabilities check. Use when asked to support a new MongoDB major or minor release line. Do not use for patch bumps inside an existing line (the weekly refresh opens those)."
---

# Adding a MongoDB version line

A MongoDB line is `<major>.<minor>` (`8.0`). It takes patches of its release only. The weekly refresh never moves a pin out of its line.

## The versions.json entry

1. Copy the newest `mongodb-*` entry under `images` to the key `mongodb-<line>`. Set `line`, `release` (the exact official tag), and `base.tag` to the official tag, keeping the suffix the other lines use.
2. Set `base.digest`. Get it from `docker buildx imagetools inspect <image>:<tag> --format '{{json .Manifest}}' | jq -r .digest`.
3. Set every package version to `"0"`, then run `$IMAGES refresh mongodb-<line>`. It pins the packages and moves the base, copies, modules and sources to the newest patch of the line. It may also move the shared `go` digest: revert that unless you mean to rebuild every image.
4. Run `$IMAGES check`. It fails when a key isn't `<engine>-<line>`, when a pin left its line, or when `sdk` differs from `go.mod`.

## What differs per line in this engine

The image sets `DATABASIFY_LINE` from the line, and the engine reads it where releases differ:

- `engine/mongodb.go` reads `DATABASIFY_LINE`. Continuous migration (Percona ClusterSync, `sources.pcsm`) needs the source on the same major as the line (`engine/migration.go`). Otherwise it offers only a one-shot copy.
- Backups use Percona Backup for MongoDB (`sources.pbm`). Check the PBM release notes for the new line. When PBM doesn't support it yet (PBM 2.17 has no 9.0), still add the line and say so in the pull request, so the app registers it with `'recovery' => false`, as it does for 9.0.
- Keep `GLIBC_TUNABLES=glibc.pthread.rseq=1` in the image `ENV` (SERVER-121912).
- `smoke/check.sh` must tolerate fields older releases lack (`?? false`).
- An older base may lack tools from the tool contract (`bash ps ss ip jq openssl curl`). Inspect the base and add only what's missing to `packages`.

Add or extend a Go test for each per-line branch you add.

## Check the new line

`$IMAGES` stands for `tools/images.sh` in a clone of [engine-sdk](https://github.com/databasifylabs/engine-sdk) at the SDK version this repository's `go.mod` requires. Run it from this repository's root:

```bash
go vet ./... && go test -race -count=1 ./...
$IMAGES check
$IMAGES build mongodb-9.1 --load --tag smoke:mongodb-9.1
$IMAGES smoke smoke:mongodb-9.1 mongodb
echo '{}' | docker run --rm -i --network none --entrypoint databasify smoke:mongodb-9.1 engine capabilities | jq -S .
echo '{}' | docker run --rm -i --network none --entrypoint databasify smoke:mongodb-9.1 config fields | jq -S .
```

- `smoke` runs `images/mongodb/smoke/check.sh` against the new line, then the SDK's conformance suite.
- Compare `engine capabilities` and `config fields` with the closest existing line. Every difference should be one you meant, such as a password encryption or setting the new release adds.
- Also run the CI scan (`TRIVY_IMAGE` in the SDK's `.github/workflows/engine-images.yml`) on `smoke:mongodb-9.1`.
- Put the results in the pull request with the line and OS you ran them on.

## The pull request

Open an issue first, branch from `main`, and open one pull request against `main` with the `versions.json` entry, any per-line code and tests, and the README's Lines section. The app registers the new line in its own repository ([databasifylabs/databasify](https://github.com/databasifylabs/databasify)), after this one merges.
