# Contributing to Codvian Router

Thank you for considering a contribution. Codvian Router is open source
(MIT), and contributions of all sizes are welcome — code, docs, tests,
bug reports, and ideas.

## Getting started

1. **Fork** the repository and clone your fork.
2. Create a branch from `main` for your change:
   `git checkout -b feat/short-description` (or `fix/...`, `docs/...`).
3. Make your change in small, focused commits.
4. Run the tests / checks for the area you touched (see the project docs
   once the codebase lands — for now, the CI sanity check must pass).
5. Push your branch and open a **pull request** against `main`, using the
   pull request template. Link any related issue.

## Pull request expectations

- One logical change per PR. Keep PRs small and reviewable.
- Describe what changed and why, and how you tested it.
- Add or update tests for behaviour changes. A PR that changes routing
  or failover behaviour without a test will be asked for one.
- Update docs (README / ROADMAP) when user-visible behaviour changes.
- **Never commit secrets.** No `.env` files, API keys, tokens, or
  credentials — in code, tests, fixtures, or logs. Use placeholders.

## Code style

- Follow the existing style of the file you are editing.
- Keep functions small and names descriptive. Prefer clear code over
  clever code — this project is built to be readable.
- Do not add dependencies without a reason stated in the PR.

## Sign-off

By contributing, you agree your contribution is licensed under the MIT
License (see [LICENSE](LICENSE)). Please sign off your commits
(`git commit -s`) to confirm you have the right to contribute the change
under the project's license.

## Good first issues

Look for issues labelled `good first issue` or `help wanted`. If you are
unsure where to start, comment on the issue and a maintainer will help.
Small docs and test improvements are a great first contribution.

## Questions

Open a discussion (or an issue, if it is a bug or feature request) rather
than emailing maintainers directly. Please read the
[Code of Conduct](CODE_OF_CONDUCT.md) — be kind, be specific, assume
good faith.
