# Contributing to sbx-acp

Bug reports and pull requests are welcome. This page says how to build the tool, which rules a change must keep, and how to send one.

## Build and test

You need Go 1.26.11 or later, the version that `go.mod` declares.

```
go build -o sbx-acp .
go vet ./...
gofmt -l .
go test ./... -count=1
```

`gofmt -l .` must print nothing. The tests need `sh` on `PATH`. One test starts the real `claude-agent-acp` adapter and is skipped on machines that do not have it.

Every pull request also runs these in GitHub Actions, from `.github/workflows/ci.yml`. The lint job runs `golangci-lint` with the settings in `.golangci.yml`. The build job compiles for Linux, macOS, and Windows. The test job runs the tests with the race detector on Linux and macOS.

## Rules a change must keep

- Use public surfaces only. Those are the documented `sbx` CLI, the Kit spec, the Go module `github.com/docker/sandbox-kit-spec/v3`, the documented Docker Hub API, and the registry API of the OCI distribution spec. Do not import internal Go packages, and do not open the sandboxd socket.
- Write nothing to stdout except the agent's bytes. Errors, warnings, `--check`, and `--help` go to stderr.
- Keep `resolve.go` the only file that imports the spec module.
- Refuse when the tool cannot know the launch. Print one sentence that says why. Do not substitute a default.
- Look up kits when the tool runs. Do not add a list of kit names, tags, or digests to the code.
- Remove a sandbox only after this process created it.
- Test what a user or an ACP client can observe: stdout bytes, stderr text, exit codes, the arguments that `sbx` received, and the requests that Docker Hub received. Tests must not use the network.

## Send a change

1. Create a branch named after a Conventional Commits type, for example `fix/truncated-read`.
2. Write commit subjects in the Conventional Commits form `type: subject`.
3. Run the four commands in [Build and test](#build-and-test).
4. Open a pull request. Say why the change exists, what you ran, and what you could not verify.

## Report a bug

Open an issue with the command you ran, the `sbx-acp:` line from stderr, the output of the same command with `--check`, and your `sbx` version.

Report a vulnerability the way [SECURITY.md](SECURITY.md) describes, not in a public issue.

## Conduct

This project follows the [Contributor Covenant Code of Conduct](CODE_OF_CONDUCT.md).

## License of contributions

The project is licensed under the [Apache License 2.0](LICENSE). Unless you state otherwise, a contribution that you submit is under the same license, as section 5 of the license describes.
