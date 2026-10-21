# Repository instructions

- Keep source code, tests, bundled default skills, example configuration, build
  and deployment scripts, and `.github/workflows/` in the repository.
- `data/` and `.codegraph/` are local-only. Never stage, force-add, commit, or
  upload their contents. Preserve local runtime data, dependencies, and indexes.
- Exclude real configuration, credentials, logs, backups, caches, and compiled
  artifacts. Publish installation archives and checksums through GitHub Releases.
- `doc/` and `docs/` contain local documentation. Never stage, force-add, commit,
  or upload files from either directory to GitHub. Preserve their local files.
- Keep public release notes in `CHANGELOG.md`. GitHub Actions must generate
  Release notes from the matching version section without reading `doc/` or `docs/`.
- In repositories with a usable `.codegraph/` index, use CodeGraph before text
  searches or file reads to understand or locate code. If no usable index exists,
  use normal code navigation tools; do not initialize an index yourself.
