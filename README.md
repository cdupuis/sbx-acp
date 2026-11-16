# sbx-acp

`sbx-acp` starts the Agent Client Protocol (ACP) agent of a Docker sandbox on stdio, so an ACP client such as Zed can use one command as its agent server. It has two subcommands.

- `sbx-acp attach SANDBOX` uses a sandbox that already exists.
- `sbx-acp create AGENT` makes a new sandbox for each session, with the ACP mixin the agent needs.

Both check that the sandbox declares the `com.docker.sandbox/acp@1` capability, then hand your stdin and stdout to the agent.

## Install

```
go build -o sbx-acp .
```

Put the binary on your `PATH`. The `go.mod` declares Go 1.26.11, the version this was built with. The kit spec module it depends on, `github.com/docker/sandbox-kit-spec/v3` at `v3.0.0-m.12`, declares Go 1.26.10. The build needs a Go toolchain at least that new. The module is pre-stable, so the pin is deliberate.

## Usage

```
sbx-acp attach [--agent NAME] [--sbx PATH] [--check] SANDBOX
sbx-acp create [--kit REF]... [--rm] [--sbx PATH] [--check] AGENT [PATH...]
```

Flags can go before or after the positional arguments, as with `sbx`. `sbx-acp --help` and `sbx-acp COMMAND --help` describe them. Nothing except the agent's protocol bytes reaches stdout. Errors, warnings, `--check`, and `--help` all write to stderr.

Both subcommands take these flags.

- `--sbx PATH` names the `sbx` executable. The default is `sbx` found on `PATH`. Use it when your ACP client starts with a short `PATH`, which is common for apps launched from a macOS Dock.
- `--check` prints what the command would do on stderr, starts nothing, and exits 0.

### attach

`attach` starts the agent of an existing sandbox. `--agent NAME` picks the agent when the sandbox declares more than one.

```json
{
  "agent_servers": {
    "Claude in my-sandbox": {
      "type": "custom",
      "command": "sbx-acp",
      "args": ["attach", "my-sandbox"],
      "env": {}
    }
  }
}
```

### create

`create` makes a sandbox named `acp-<agent>-<8 hex digits>` for a built-in agent such as `claude`, `codex`, or `opencode`, then starts that agent in it. Every run makes a new sandbox.

- `--rm` removes the sandbox when the session ends. Without it the sandbox stays, and stderr names it.
- `--kit REF` names a mixin for `sbx create --kit`. You can repeat it. If you give it, `sbx-acp` adds exactly those mixins and does not look one up.
- `PATH...` are the workspaces, in the form `sbx create` takes. Without a `PATH` the workspace is the working directory. `sbx-acp` refuses to default to `/` or your home directory.

```json
{
  "agent_servers": {
    "Claude in a new sandbox": {
      "type": "custom",
      "command": "sbx-acp",
      "args": ["create", "--rm", "claude"],
      "env": {}
    }
  }
}
```

Zed starts a custom agent server in the project root. `sbx` mounts a workspace at the same path as on the host. So the directory that Zed sends when it opens a session exists inside the new sandbox.

`sbx-acp create --check claude` shows the mixin it found and the `sbx create` command it would run.

## How it works

Every Kit stages its published descriptor at `/usr/share/sandbox/kit/<name>/kit.yaml` inside the sandbox. `sbx-acp` reads those files, checks that the resolved command is executable, and starts the agent through three `sbx exec` calls.

```
sbx exec SANDBOX sh -c '<script that prints every kit.yaml, framed with NUL bytes>'
sbx exec SANDBOX sh -c '<script that checks the command file and its execute permission>' sbx-acp COMMAND [PATH]
sbx exec -i [-e KEY=VALUE]... SANDBOX COMMAND [ARG...]
```

The first call has no stdin and no TTY. The script needs only `sh` and `cat`. A stream without its end marker counts as a truncated read and fails.

The executable check also gets no stdin or TTY. It uses the sandbox's default user and working directory. It searches the launch's `PATH` when the descriptor overrides it. `--check` resolves the descriptor without checking whether the executable exists.

The third call is the stream. `sbx-acp` gives `sbx` its own stdin and stdout descriptors, so no copy loop sits between the client and the agent. Environment values from the descriptor go through `-e`, sorted by key. `sbx-acp` never passes `-t`. It forwards SIGINT and SIGTERM during discovery, the executable check, and the stream. An interrupt before the stream exits with 128 plus the signal number, even if `sbx` exits 0. Once the stream starts, `sbx-acp` returns the code from `sbx`, or 128 plus the signal number if a signal ended `sbx`.

`Resolve` in `resolve.go` applies the capability contract to the decoded descriptors.

1. It decodes each descriptor with `spec.Decode`. A descriptor that fails is skipped with a warning and named in any later "no agent" error.
2. It collects `provides` from every kit, so a mixin can declare the launch for an agent that another kit provides.
3. It reads `acp@1` entries one at a time, including entries inside capability groups.
4. It keys entries on the normalized agent name. Identical restatements collapse. Different launches for one agent are a conflict.
5. It treats an agent as a candidate only if some kit provides it.
6. Without `--agent`, exactly one candidate must exist. A broken candidate still counts, so fixing it never changes which agent a bare command starts.

`protocolVersion` is shown by `--check` and not enforced. The ACP client negotiates the version in `initialize`, and `sbx-acp` cannot know which version the client speaks.

### How create works

Some agents declare ACP in their own kit. Others need an adapter mixin. `create` finds the mixin each time it runs, because the set of published kits changes.

1. It lists the repositories in the Docker Hub namespace `sbx` whose names contain `acp`. Built-in agent names resolve to kits in that namespace, so its mixins are built for them.
2. It keeps the repositories that hold a current-format kit and have the agent name in their name. This only limits what it reads next.
3. It reads the kit descriptor of each from the registry and selects the mixin that declares an `acp@1` launch for the agent. Two such mixins are an error. None is not an error, because the agent's own kit may declare ACP.
4. It runs `sbx create --no-progress --name NAME [--kit REF] AGENT PATH...`. `sbx create` gets no stdin, and its output goes to stderr.
5. It reads the new sandbox the way `attach` does and starts the agent. The sandbox has the final word on whether the agent speaks ACP.

`sbx-acp` removes a sandbox only if its own `sbx create` succeeded, and only in two cases. Either you passed `--rm`, or discovery, launch resolution, or the executable check failed before the stream started. It never removes a sandbox after a failed `sbx create`, because that failure does not prove the name is its own.

After the streaming `sbx exec` starts, its status cannot prove whether the agent ran. For example, an agent can return 127 itself. Without `--rm`, `sbx-acp` retains the sandbox after a stream attempt, including failures that the executable check cannot detect.

## What counts as public

The tool uses five things.

- The `sbx` CLI, as documented at https://docs.docker.com/reference/cli/sbx/. It calls `sbx exec`, `sbx create`, and `sbx rm`.
- The Kit spec, including the staged descriptor location in section 10 of `SPEC-v3.md`, the descriptor annotation on a published kit, and the `acp@1` capability page.
- The Go module `github.com/docker/sandbox-kit-spec/v3`, which `resolve.go` alone imports.
- The Docker Hub API, as documented at https://docs.docker.com/reference/api/hub/latest/. It calls the repository list of one namespace, without credentials.
- The registry API of the OCI distribution spec, with an anonymous pull token. It reads one manifest for each candidate mixin.

It never opens the sandboxd socket and imports no internal Go package. Those are internal, so a client built on them would break without notice. `sbx inspect`, `sbx ls`, and `sbx kit inspect` do not print capabilities, which is why the tool reads the descriptors itself.

## Exit codes

| Code | Meaning |
|---|---|
| 2 | Usage error. |
| 3 | No launchable ACP agent for the request, in the sandbox or among the published mixins. |
| 4 | `create` could not read the kit catalog on Docker Hub. |
| 125 | `sbx` is missing, `sbx create` failed, or a sandbox read or executable check failed. |
| 127 | The declared launch command is missing or not executable. |
| other | After the stream starts, the code that `sbx exec` returned. |

`--help` exits 0. A failure prints `sbx-acp: sandbox "NAME": <sentence>` on stderr, or `sbx-acp: agent "NAME": <sentence>` before `create` has a sandbox. Usage errors omit that part.

## Limits

- Entries that use kit arguments or kit environment references are refused. The values are chosen at sandbox creation and are not readable from outside, and substituting defaults could start a different command than the runtime would.
- The sandbox image needs `sh` and `cat`.
- The executable check cannot detect a missing interpreter or dynamic loader, a failure inside a shell command, or a file that changes after the check. Use `--rm` to remove a newly created sandbox after those stream failures.
- `sbx exec` may give the command a different working directory or user than an agent session would. The capability describes launch under an agent session. This tool has not checked how the two differ.
- The real `sbx` binary was not available where this was written. The `sbx exec`, `sbx create`, and `sbx rm` calls were exercised only through a stand-in that implements their documented usage. These assumptions are taken from the published reference and are not verified: `sbx` passes the arguments after `SANDBOX` through untouched, it half-closes stdin on EOF, and `sbx create --kit sbx/claude-acp:latest claude` composes the mixin onto the built-in agent.
- `create` leaves a sandbox behind unless you pass `--rm`. A process that is killed with SIGKILL leaves one even with `--rm`. `sbx ls` shows them under names that start with `acp-`, and `sbx rm NAME` removes one.
- `--rm` deletes everything inside the sandbox, including the agent's session history. Files in the workspace are on the host and stay.
- The first `sbx create` for an agent pulls images and can take minutes. The ACP client waits for its first reply during that time and may give up.
- `create` passes only `--name`, `--kit`, the agent, and the paths to `sbx create`. For any other option, create the sandbox with `sbx create` and use `attach`.
- The lookup reads only the namespace `sbx`. If an `sbx` setting points a built-in agent name at another kit, the mixin it finds may not fit. Name the mixin with `--kit`.
- Each lookup that finds a candidate reads its manifest without credentials. Docker Hub counts that against the anonymous pull limit of your network address. `--kit` skips the lookup.
- The default workspace is right only when the client starts `sbx-acp` in the project directory. Zed does. For a client that does not, pass a `PATH`.
- The spec module is pre-stable. A descriptor written for a newer schema may fail to decode, and `sbx-acp` then skips that kit with a warning.

## Tests

```
go test ./... -count=1
```

The tests build the binary and run it against a fake `sbx`, which is the test binary started under the name `sbx`. The lookup tests run against a local HTTP server with fixtures of real kits. No test uses the network. One test starts the real `claude-agent-acp` adapter when it is on `PATH` and checks that it answers an `initialize` request. That test is skipped on machines without the adapter.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) says how to build, test, and send a change. Report a vulnerability the way [SECURITY.md](SECURITY.md) describes. The project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).

## License

`sbx-acp` is licensed under the [Apache License 2.0](LICENSE).
