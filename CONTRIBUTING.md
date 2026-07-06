# Contributing

Issues and pull requests are welcome.

## Development

This project targets macOS on Apple Silicon. Some packages and tests require `darwin/arm64`.

Useful commands:

```sh
make generate format
make lint
make test
make build
```

Before opening a pull request, run the commands that match your change and commit any generated updates.

## Pull Requests

Keep pull requests focused and include tests when changing behavior.

CI runs automatically for branches in this repository. Fork pull requests are welcome, but end-to-end CI jobs do
not run directly from cross-repository pull requests. Before merging such a pull request, run the end-to-end jobs
locally, import the exact PR head into a branch in this repository so CI can run, or let the same coverage run on
`main` after merge when that risk is acceptable.

## Maintainer Fork Verification

Use this flow when a fork pull request is ready for repository CI verification:

```sh
gh pr checkout <pr-number> -b review/pr-<pr-number>
git push origin review/pr-<pr-number>
```

Then open a pull request from `review/pr-<pr-number>` to `main`. The repository CI will run on that branch.

Verify the imported branch points at the expected pull request head before relying on the CI result:

```sh
gh pr view <pr-number> --json headRefOid
git rev-parse review/pr-<pr-number>
```
