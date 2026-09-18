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

Initialize a local database and verify the installation:

```bash
memgov init
memgov doctor
memgov version
```

Copy `config.local.yaml.example` to `~/.memgov/config.yaml` only when you need runtime or DingTalk integration. Keep credentials out of the repository.

## Build from source

Building requires the Go version declared in `go.mod`, Node.js 22.16 or newer, and npm.

```bash
git clone https://github.com/zhoushoujianwork/memgov.git
cd memgov
make check
make install
./.memgov/bin/memgov init
```

The optional AI runtime also requires the external tools configured for that runtime, such as Claude Code and `dws`. Core local memory commands do not require DingTalk credentials or a model API key.

For a long-running macOS service, see [the runtime guide](docs/guides/runtime-user-guide.md). For development and configuration details, see [README](README.md) and [initialization](docs/guides/initialization.md).
