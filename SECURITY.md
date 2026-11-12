# Security policy

## Supported versions

Security fixes go to `dev` and ship in the next release from `main`. Only the newest
release receives fixes.

## Reporting a vulnerability

Do not open a public issue. Report it privately through GitHub's private
vulnerability reporting: the "Report a vulnerability" button on the repository's
Security tab. Include the version or commit, a description, and steps or a test that
reproduces it.

You get an acknowledgement within 7 days. A fix and an advisory follow once the
issue is confirmed; reporters are credited unless they ask not to be.

## Scope

Corvid is a library. The boundaries it promises are the ones its documentation
states, above all the permission pipeline that every tool call passes through (see
the [permissions guide](docs/guides/permissions.md)) and the fail-closed tool
defaults (see the [tools guide](docs/guides/tools.md)). A way around any of them is in
scope. How a host configures its tools, modes and rules is the host's responsibility.
