# Releasing

1. In `CHANGELOG.md`, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, add a fresh empty
   `## [Unreleased]` above it, and update the compare links at the bottom.
2. Optionally write user-facing notes in `docs/releases/vX.Y.Z.md` (what's new, upgrade notes,
   image tags). Without that file the release uses the version's `CHANGELOG.md` section.
3. Merge to `main`, then tag and push:

   ```bash
   git tag -a vX.Y.Z -m "vX.Y.Z"
   git push origin vX.Y.Z
   ```

The tag push triggers two workflows: `docker-publish.yml` publishes
`ghcr.io/guibibi/tordownloader:vX.Y.Z` and moves `:latest`, and `release.yml` creates the GitHub
Release from the notes. To create or refresh the release for a tag that already exists, run
**Actions → Release → Run workflow** on `main` with that tag.
