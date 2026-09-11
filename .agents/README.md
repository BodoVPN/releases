# Shared project tooling

Manage repository instructions in `AGENTS.md` here. Both root instruction files link to that source. Add project skills under `skills/`, rules under `rules/`, and tool or plugin declarations to `tooling.json`. Personal skills and plugins remain available from `~/.agents/`.

Run `tooling sync` after changing declarations, then `tooling check`. The organization maintains one manager in `BodoVPN/bodovpn` at `.agents/manage.py`. Without the personal shortcut, run that script with Python 3.11 or newer: `python <backend-checkout>/.agents/manage.py sync --project <this-checkout>`. The checked-in links work without installing the manager; it is needed when changing declarations or adding new shared directories.

On Windows, enable Developer Mode and Git `core.symlinks` before cloning so the links are checked out correctly. Credentials and machine-specific runtime preferences are not stored here.
