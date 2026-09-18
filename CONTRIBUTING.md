# Contributing

Thanks for helping improve memgov.

## Development setup

Use the Go version declared in `go.mod`, Node.js 22.16 or newer, and npm. From the repository root:

```bash
npm ci --prefix web
make check
```

Keep changes focused, add tests for behavior changes, and update user-facing documentation when commands, configuration, architecture, or status changes. Run `git diff --check` before submitting a pull request.

## Public data rules

Never commit credentials, tokens, real chat or user identifiers, private message contents, local databases, runtime logs, machine-specific paths, or internal deployment evidence. Use synthetic fixtures and clearly fake identifiers. Keep `config.local.yaml` and the entire `.memgov/` directory local.

Security issues should follow [SECURITY.md](SECURITY.md), not a public issue.
