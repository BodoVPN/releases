# Repository guidance

This repository hosts release workflows and public download information. Read `README.md` and the relevant workflow under `.github/workflows/` before changing release behavior.

- Keep release assets, platform names, and download instructions consistent with the release workflow.
- Do not publish a release or replace uploaded artifacts unless that action is requested.
- Keep signing credentials, tokens, and local machine settings out of commits and logs.
- For configuration changes, validate the changed YAML/JSON and run `git diff --check`.
- Do not include assistant attribution or co-authored-by trailers in commits or pull requests.

Manage instructions and project-specific tooling in `.agents/`. Both root instruction files link here. Personal skills and plugins are shared through `~/.agents/`. Run `tooling sync` after changing declarations and `tooling check` before handing off. The shared manager is maintained in `BodoVPN/bodovpn` at `.agents/manage.py`.
