---
name: bump-version
description: |
  Use this skill when the user asks to cut/release/bump a new bulwark
  version (e.g. "cut a new version", "release with the recent fixes",
  "tag a release"). Covers picking the semver bump, tagging the release
  (which fires .github/workflows/release.yml → goreleaser), moving the
  floating major-alias tag (vN), and verifying the published release.
---

# Cut a bulwark release

Releases are **tag-driven**: pushing an annotated `vX.Y.Z` tag to a commit
on `main` triggers `.github/workflows/release.yml` (build & test →
goreleaser), which publishes the GitHub release with the `bulwark`
binary (linux/darwin, amd64/arm64), `checksums.txt`, and `install.sh`.

Two tags move per release and **both are mandatory**:

1. the immutable `vX.Y.Z` release tag (the workflow trigger), and
2. the floating `vN` major-alias tag (e.g. `v1`), force-moved to the same
   commit. Consumers pin `uses: wardnet/bulwark@v1`, so the alias **must**
   track the latest release of that major. This is convention, not
   optional — a release where `v1` still points at the previous commit is
   an incomplete release.

## 0. Preconditions

- The fixes to release are **already merged to `main`** and `main`'s CI is
  green. This skill does not merge PRs (see the repo CLAUDE.md rule). If the
  fix is still on a PR, stop and get it merged first.
- You are in the `gt` bare-repo layout; `gh`/`git` authenticate as the user
  in the root `.envrc` (`gh auth token --user <username>`). Read that user
  before any `gh`/push command.
- `git fetch origin --tags` so local tags and `origin/main` are current.

## 1. Pick the version

Find the current latest and what has landed since it:

```bash
gh release list --repo wardnet/bulwark --limit 5
git log --oneline "$(gh release view --repo wardnet/bulwark --json tagName -q .tagName)"..origin/main
```

Choose the bump by the **highest-impact change to the shipped binary**
since the last release (SemVer):

- **patch** (`vX.Y.Z+1`) — bug fixes only; no new user-facing behaviour.
- **minor** (`vX.Y+1.0`) — backward-compatible features / new flags / new
  scanner integrations.
- **major** (`vX+1.0.0`) — breaking changes (config schema changes, removed
  CLI surface, `bulwark-state` baseline format changes). A new major also
  means a **new floating alias** (`vN+1`) and updating consumers that pin the
  old one — call this out explicitly.

Config-only changes (e.g. a `dependabot.yml` edit) do **not** by themselves
warrant a release; only cut one when the binary changed.

Let `$VER` be the new version (e.g. `v1.3.1`) and `$MAJOR` its alias (`v1`).
Let `$SHA` be the `origin/main` commit to release.

## 2. Tag the release and push

Annotated tag (matches existing release tags), at the exact `main` commit:

```bash
git tag -a "$VER" "$SHA" -m "$VER

<one bullet per change being shipped, e.g. fix(...) (#NNN)>"
git push origin "$VER"
```

The push fires `release.yml`. Nothing else triggers it — only tags matching
`v*.*.*`.

## 3. Move the floating major alias

The `vN` tag is **lightweight and hand-maintained** (not a workflow
trigger). Re-point it and force-push:

```bash
git tag -f "$MAJOR" "$SHA"
git push origin "$MAJOR" --force
```

The force-push of a shared tag may need explicit approval — that is
expected; this step is required, so request it if blocked. Do not skip it.

## 4. Verify

```bash
gh run watch "$(gh run list --repo wardnet/bulwark --workflow release.yml \
  --limit 1 --json databaseId -q '.[0].databaseId')" \
  --repo wardnet/bulwark --exit-status
gh release view "$VER" --repo wardnet/bulwark \
  --json tagName,isDraft,isPrerelease,assets \
  -q '{tag:.tagName, draft:.isDraft, prerelease:.isPrerelease, assets:[.assets[].name]}'
git ls-remote origin "refs/tags/$MAJOR"   # must equal $SHA
```
