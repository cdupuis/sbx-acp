# sbx-acp

`sbx-acp` starts the Agent Client Protocol (ACP) agent of a Docker sandbox on stdio. It checks that the sandbox declares the `com.docker.sandbox/acp@1` capability, then hands your stdin and stdout to the agent, so an ACP client such as Zed can use one command as its agent server.

## Install

```
go build -o sbx-acp .
```

Put the binary on your `PATH`. The `go.mod` declares Go 1.26.11, the version this was built with. The kit spec module it depends on, `github.com/docker/sandbox-kit-spec/v3` at `v3.0.0-m.12`, declares Go 1.26.10. The build needs a Go toolchain at least that new. The module is pre-stable, so the pin is deliberate.

## Usage

```
sbx-acp [--agent NAME] [--sbx PATH] [--check] SANDBOX
```

- `--agent NAME` picks the agent when the sandbox declares more than one.
- `--sbx PATH` names the `sbx` executable. The default is `sbx` found on `PATH`. Use it when your ACP client starts with a short `PATH`, which is common for apps launched from a macOS Dock.
- `--check` resolves the launch, prints it on stderr, starts nothing, and exits 0.

Flags go before `SANDBOX`.

Zed `settings.json`:

```json
{
  "agent_servers": {
    "Claude in my-sandbox": {
      "type": "custom",
      "command": "sbx-acp",
      "args": ["my-sandbox"],
      "env": {}
    }
  }
}
```

Nothing except the agent's protocol bytes reaches stdout. Errors, warnings, `--check`, and `--help` all write to stderr.

## How it works

Every Kit stages its published descriptor at `/usr/share/sandbox/kit/<name>/kit.yaml` inside the sandbox. `sbx-acp` reads those files with one `sbx exec` call, resolves the ACP launch from them, and starts the agent with a second call.

```
sbx exec SANDBOX sh -c '<script that prints every kit.yaml, framed with NUL bytes>'
sbx exec -i [-e KEY=VALUE]... SANDBOX COMMAND [ARG...]
```

The first call has no stdin and no TTY. The script needs only `sh` and `cat`. A stream without its end marker counts as a truncated read and fails.

The second call is the stream. `sbx-acp` gives `sbx` its own stdin and stdout descriptors, so no copy loop sits between the client and the agent. Environment values from the descriptor go through `-e`, sorted by key. `sbx-acp` never passes `-t`. It forwards SIGINT and SIGTERM to `sbx` and exits with the code `sbx` returned. If a signal ended `sbx`, the exit code is 128 plus the signal number.

`Resolve` in `resolve.go` applies the capability contract to the decoded descriptors.

1. It decodes each descriptor with `spec.Decode`. A descriptor that fails is skipped with a warning and named in any later "no agent" error.
2. It collects `provides` from every kit, so a mixin can declare the launch for an agent that another kit provides.
3. It reads `acp@1` entries one at a time, including entries inside capability groups.
4. It keys entries on the normalized agent name. Identical restatements collapse. Different launches for one agent are a conflict.
5. It treats an agent as a candidate only if some kit provides it.
6. Without `--agent`, exactly one candidate must exist. A broken candidate still counts, so fixing it never changes which agent a bare command starts.

`protocolVersion` is shown by `--check` and not enforced. The ACP client negotiates the version in `initialize`, and `sbx-acp` cannot know which version the client speaks.

## What counts as public

The tool uses three things.

- The `sbx` CLI, as documented at https://docs.docker.com/reference/cli/sbx/. It calls only `sbx exec`.
- The Kit spec, including the staged descriptor location in section 10 of `SPEC-v3.md` and the `acp@1` capability page.
- The Go module `github.com/docker/sandbox-kit-spec/v3`, which `resolve.go` alone imports.

It never opens the sandboxd socket and imports no internal Go package. Those are internal, so a client built on them would break without notice. `sbx inspect` and `sbx ls` do not print capabilities, and no `sbx` command prints the composed result, which is why the tool reads the staged descriptors itself.

## Exit codes

| Code | Meaning |
|---|---|
| 2 | Usage error. |
| 3 | The sandbox was read and has no launchable ACP agent for the request. |
| 125 | `sbx` is missing, or the sandbox could not be reached or read. |
| other | After the stream starts, the code that `sbx exec` returned. |

`--help` exits 0. Every failure prints `sbx-acp: sandbox "NAME": <sentence>` on stderr. Usage errors omit the sandbox part.

## Limits

- Entries that use kit arguments or kit environment references are refused. The values are chosen at sandbox creation and are not readable from outside, and substituting defaults could start a different command than the runtime would.
- The sandbox image needs `sh` and `cat`.
- `sbx exec` may give the command a different working directory or user than an agent session would. The capability describes launch under an agent session. This tool has not checked how the two differ.
- The real `sbx` binary was not available where this was written. The `sbx exec` calls were exercised only through a stand-in that implements the documented usage `sbx exec [flags] SANDBOX COMMAND [ARG...]`. The assumptions that `sbx` passes the arguments after `SANDBOX` through untouched, and that it half-closes stdin on EOF, are taken from the published reference and are not verified.
- The spec module is pre-stable. A descriptor written for a newer schema may fail to decode, and `sbx-acp` then skips that kit with a warning.

## Tests

```
go test ./... -count=1
```

The tests build the binary and run it against a fake `sbx`, which is the test binary started under the name `sbx`. One test starts the real `claude-agent-acp` adapter when it is on `PATH` and checks that it answers an `initialize` request. That test is skipped on machines without the adapter.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) says how to build, test, and send a change. Report a vulnerability the way [SECURITY.md](SECURITY.md) describes. The project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).

## License

`sbx-acp` is licensed under the [Apache License 2.0](LICENSE).
