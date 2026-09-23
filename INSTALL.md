# Install memgov

## Release archive

Download the archive for your operating system and CPU from the repository's GitHub Releases page, plus `SHA256SUMS`. Verify the archive before extracting it.

```bash
# macOS example
shasum -a 256 -c SHA256SUMS
tar -xzf memgov-VERSION-darwin-arm64.tar.gz
install -d "$HOME/.local/bin"
install "memgov-VERSION-darwin-arm64/memgov" "$HOME/.local/bin/memgov"
export PATH="$HOME/.local/bin:$PATH"
```

Linux users can verify with `sha256sum -c SHA256SUMS` and select the `linux-amd64` or `linux-arm64` archive.

For a new data home, initialize a local database and verify the installation. Existing installations must follow the upgrade steps below first:

```bash
memgov init
memgov doctor
memgov version
```

Copy `config.local.yaml.example` to `~/.memgov/config.yaml` only when you need runtime or DingTalk integration. Keep credentials out of the repository.

## Upgrade from the retired memory system

Stop the old service before replacing its executable or changing configuration. Keep its matching binary for rollback. With the new executable, use the effective configuration and data home:

```bash
memgov --home /path/to/data --config /path/to/config.yaml config migrate-workspaces
memgov --home /path/to/data --config /path/to/config.yaml init
memgov --home /path/to/data --config /path/to/config.yaml doctor
memgov --home /path/to/data --config /path/to/config.yaml config plan
```

Configuration conversion archives the original YAML and legacy AgentHome notes before removing retired fields. It does not apply runtime declarations. Schema 27 then archives and verifies the complete old database and legacy knowledge/configuration bundle before removing the old memory pipeline. Archive or verification failure blocks destructive migration. Managed-service upgrades use the same gate, with configuration conversion required first.

New Agent Workspaces start empty; archived Memory and AgentHome notes are not imported. Review the configuration plan before explicitly applying changes and starting the service. Rollback requires the verified pre-upgrade archive and the corresponding old binary. See [upgrade and recovery](docs/guides/governance.md).

Normal `backup` commands cover operational SQLite data only. For complete recovery, separately preserve `agent-workspaces/`, `agent-workspace-history/`, effective configuration and independently managed presets/secrets while writers are stopped.

## Build from source

Building requires the Go version declared in `go.mod`, Node.js 22.16 or newer, and npm.

```bash
git clone https://github.com/zhoushoujianwork/memgov.git
cd memgov
make check
make install
./.memgov/bin/memgov init
```

On macOS, an ad hoc signed local build can lose its Files & Folders permission after a rebuild. If the service needs access to a protected folder, sign successive builds with the same valid Apple Development or Developer ID identity. Put `CODESIGN_IDENTITY := <identity SHA-1>` in the ignored `.memgov/signing.mk` before running `make install`; find an available identity with `security find-identity -p codesigning -v`. The build signs the new binary before replacing the previous one. Grant only the folder access needed in System Settings > Privacy & Security > Files & Folders. An Agent should use its configured project workspace rather than search the whole user home to locate a project.

The optional AI runtime also requires the external tools configured for that runtime, such as Claude Code and `dws`. Local Workspace reads and writes do not require DingTalk credentials or a model API key; runtime identity checks still require the configured, verified task context.

For a long-running macOS service, see [the runtime guide](docs/guides/runtime-user-guide.md). For development and configuration details, see [README](README.md) and [initialization](docs/guides/initialization.md).
