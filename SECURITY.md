# Security policy

## Supported versions

Only the latest commit on `main` receives fixes. The project has no tagged releases.

## Report a vulnerability

Do not open a public issue for a vulnerability.

If the repository shows a **Report a vulnerability** button under its **Security** tab, use that form. If it does not, contact the repository owner through the details on their GitHub profile, https://github.com/cdupuis.

Include the commit you built from, your `sbx` version, the steps that reproduce the problem, and what an attacker gains.

## Scope

`sbx-acp` runs on the host. `attach` reads the Kit descriptors of one sandbox and starts the command that those descriptors declare, inside that sandbox, through `sbx exec`. `create` first makes the sandbox through `sbx create`, with a mixin that you name or that it finds in the Docker Hub namespace `sbx`.

These are vulnerabilities in `sbx-acp`:

- It runs a program on the host other than `sbx`.
- It starts a command in the sandbox that no descriptor declares for the selected agent.
- It writes bytes to stdout that did not come from the agent.
- It adds a mixin to a new sandbox that you did not name and that is not published in the namespace `sbx`.
- It removes a sandbox that it did not create.

Problems in `sbx`, in the sandbox runtime, in a Kit, or in the agent itself belong to those projects. Report them there.
