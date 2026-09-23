# memgov Owner Assistant

memgov is a local personal work assistant with Owner private chat, DWS-backed proactive processing and independent group Agents. It combines current requests, permitted message history, project tools and persistent Agent Workspace knowledge. Communication platforms and execution harnesses are separate adapter boundaries; current integrations and validation limits are listed in [implementation status](docs/implementation-status.md).

**Workspace files hold knowledge. SQLite holds operational state.** Owner private chat and background work share the verified Owner's workspace; each group has a separate workspace even when groups share an Agent preset. Messages, internal Source evidence, tasks, permissions, confirmations, delivery and recovery remain in `state.db`.

The old Candidate/Review/Memory pipeline, memory-card UI, publication model and `memgov-memory` skill are replaced by [Agent Workspace](docs/design/agent-workspace-design.md). Upgrades archive old knowledge and start empty workspaces; they do not import old Memory or AgentHome notes. Source evidence remains internal for message behavior and retention.

## Build and start

Release installation instructions are in [INSTALL.md](INSTALL.md). Development requires the Go version in `go.mod`, Node 22.16+ and npm. End users run one executable and do not need the frontend toolchain.

```bash
make install
.memgov/bin/memgov init
.memgov/bin/memgov config show
.memgov/bin/memgov doctor
```

For an existing installation, convert the effective old configuration before the schema upgrade:

```bash
memgov --home /path/to/data --config /path/to/config.yaml config migrate-workspaces
memgov --home /path/to/data --config /path/to/config.yaml init
```

Stop the old service before conversion. Conversion archives the original configuration and AgentHome notes, removes obsolete fields and validates the result; it does not apply runtime state. Schema 27 makes and verifies a consistent old database archive before destructive migration. See [initialization](docs/guides/initialization.md) and [recovery](docs/guides/governance.md). Never run the old executable against the upgraded schema.

The active configuration defaults to `~/.memgov/config.yaml`; `config.dual.yaml` is only a migration backup. A repository `config.local.yaml` can be a development link, rather than a second active declaration. The [configuration example](config.local.yaml.example) shows platform and Agent settings. Use `config plan` and its returned version/digest before `config apply-runtime`.

macOS managed operation uses `memgov service install --config ~/.memgov/config.yaml`. `service status`, `stop` and `start` manage it; foreground debugging uses `service start` without an installed manager. Building or merging this refactor does not itself authorize installation or service replacement. See [unified service](docs/design/unified-service-design.md).

## Persistent knowledge

A workspace starts with a short `MEMORY.md` index and `notes/`, `projects/`, `daily/` directories. Each turn gets the latest bounded index; detailed files are searched and read when needed. Agents can maintain dated notes and source references directly. Successful writes are read back before reporting that knowledge was saved. Concurrent changes require a current content digest and a fresh read/merge on conflict.

The local owner can inspect workspaces using the console or CLI:

```bash
memgov agent workspace list
memgov agent workspace list --workspace-id WORKSPACE_ID
memgov agent workspace read --workspace-id WORKSPACE_ID --path MEMORY.md
memgov agent workspace search --workspace-id WORKSPACE_ID --query 'release procedure' --limit 10
memgov agent workspace history --workspace-id WORKSPACE_ID --path notes/release.md
```

Replace `WORKSPACE_ID` with an ID from the list. Runtime tools derive workspace identity from the verified task and audience; model text cannot select another Owner or group. No-Bash group Agents can use scoped knowledge tools without getting general filesystem access. Shared references require explicit read-only directory configuration. Knowledge text cannot grant shell, messaging or production permissions. Full Bash still uses the service account's permissions and is not an OS sandbox.

Task scratch, sessions, project checkouts, code worktrees and Agent presets remain separate from durable knowledge. New sessions and service restarts do not clear workspace files. Raw message retention and note retention are distinct; copying whole chat logs into notes is not the knowledge model.

## Operational interfaces

The local console provides `/tasks`, `/workspaces`, `/running`, and `/settings`. Workspace browsing is read-only and includes file search, safe Markdown reading and revision metadata. Service restart and safe task continuation use existing service controls. The console is local-only, not a multi-user authenticated service. See the [console guide](docs/guides/local-console-user-guide.md).

| Command family | Purpose |
| --- | --- |
| `init`, `config`, `doctor` | Initialization, explicit configuration conversion, planning, application and diagnosis |
| `agent workspace` | Persistent knowledge files and revision history |
| `agent preset` | Versioned execution rules |
| `workspace` | Project path associations; these do not define Agent knowledge ownership |
| `channel`, `message`, `outbox` | Platform intake, source evidence, routes and delivery |
| `data-source` | Independent collection, coverage and retention |
| `runtime`, `runtime task`, `runtime logs` | Execution configuration, task control, attempts and diagnostics |
| `service`, `ui` | Unified/managed service and local console |
| `backup`, `version`, `completion` | Operational backup, executable version and shell completion |

Exact flags and JSON envelopes are in the [CLI contract](docs/reference/cli-contract.md) and the installed command's help. Data home selection remains `--home`, then `MEMGOV_HOME`, then `~/.memgov`. Workspace knowledge has no global automatic merge or published-card scope.

Proactive completion currently records results locally (`record_only`). Independently authorized Agent communication remains a separate auditable action. Personal root/child task orchestration and automatic proactive result notifications remain product goals, not capabilities delivered by this storage refactor. Group replies keep their original-group route and do not inherit Owner private context.

## Data protection and validation

A complete recovery set includes the operational database, effective configuration, current workspace files and revision history. A database-only backup cannot restore file knowledge. Migration archives stay outside Agent knowledge mounts. Rollback uses the pre-upgrade archive and matching old executable. See [governance and recovery](docs/guides/governance.md).

```bash
make check
make test-runtime-race
scripts/runtime-offline-acceptance.sh
npm --prefix web run build
```

Tests use disposable homes and synthetic data. Passing source checks does not establish real credentials, harness execution, platform delivery or user acceptance. The [scenario standard](docs/architecture/best-practice-scenarios.md) measures discover → investigate → handle → accept → reuse. Unknown external results must be verified before replay; task completion, platform acceptance and user acceptance are different facts.

Frontend development uses `make web-dev` against an isolated local service; details are in [web/README.md](web/README.md). Compiled assets are embedded in the Go binary. Structural runtime logs are bounded and avoid chat bodies or credentials; separately managed process output follows task/version/source visibility checks.

## Agent skill and documentation

The repository and runtime provide [memgov-workspace](.agents/skills/memgov-workspace/SKILL.md). It describes workspace read/search/write/history with current-digest checking. The runtime binds identity and authority; the skill is guidance, not an authorization grant. The repository skill is the maintained source for external Agent use; old `memgov-memory` installations must be removed rather than reintroduced through skill inheritance.

Start with [documentation navigation](docs/README.md), [architecture](docs/architecture/architecture.md), [Workspace design](docs/design/agent-workspace-design.md), [runtime guide](docs/guides/runtime-user-guide.md), [status](docs/implementation-status.md) and [roadmap](docs/roadmap.md). Superseded designs remain labelled historical and do not define current commands.

## License

memgov is available under the [Apache License 2.0](LICENSE).
