# memgov local Web console

React, TypeScript and Vite power four pages: tasks, Agent Workspaces, runtime status, and settings. The console uses the local `/api/v1` API. Workspace browsing is read-only and shows current files, search results, Markdown content, content digests, and revision metadata. Owner and group workspaces remain separate; search always targets the selected workspace.

Development requires Node 22.16+ and npm. Start an isolated local memgov service, then run from the repository root:

```bash
make web-dev
```

Open `http://127.0.0.1:5178/`. Vite watches `web/src/` and proxies `/api/v1`, including SSE, to `127.0.0.1:8787`. The proxy translates only its own loopback development origin; production Host, Origin, and control-request checks remain in place. The development console operates on whichever instance is listening on port 8787, so use a temporary data directory for tests.

```bash
make web-check   # Type checking and frontend tests
make build       # Frontend assets and Go binary
make check       # Frontend and Go checks
```

`internal/console/static/` contains tracked production assets embedded into the Go binary. Regenerate them with `npm --prefix web run build` after frontend edits; do not edit generated assets directly. Building does not install the executable or restart a service. End users do not need Node or Vite.

- `src/pages/`: tasks, workspaces, runtime status, and settings.
- `src/components/`: common controls, safe Markdown reading, and DOM adapters.
- `src/types.ts`, `src/api.ts`: presentation types and API client.
- `src/shared/`: API, time, Markdown, and platform helpers.
- `src/adapters/`: YAML editing and the read-only SSE terminal.

The retained attribution for earlier Relayer components is in [LICENSE-relayer](LICENSE-relayer) and embedded in production assets. See the [console design](../docs/design/local-console-design.md) and [implementation details](../docs/design/local-console-design-detail.md) for the full console boundary.
