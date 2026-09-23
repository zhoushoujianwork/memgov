# Agent Workspace implementation details

Scope is defined by the [main design](agent-workspace-design.md). This file records the implementation contract; source validation and deployment status are reported separately in [implementation status](../implementation-status.md).

## Ownership, layout, and access

The data home contains `agent-workspaces/<workspace-id>/`, distinct from presets, legacy AgentHome archives, sessions, task scratch, project checkouts, and code worktrees. `OwnerPrincipalID` selects Owner knowledge; group identity is the pair `channel_id + conversation_id`. IDs are derived by the storage package, not selected by model text or Agent preset names.

Each workspace has metadata plus `MEMORY.md`, `notes/`, `projects/`, and `daily/`. Internal metadata and revision storage do not appear as knowledge files. Markdown files are the knowledge authority; any future search index must be rebuildable. SQLite may record workspace operations and digests, but not a second authoritative copy of note content.

Only the latest bounded index is supplied every turn, including an already-active Owner chat after background work changes the index. Other files are read on demand. Source references and observation dates are content maintained by the Agent; they do not recreate a mandatory source ingestion or candidate approval pipeline. Raw source text, knowledge, and tool output remain context data and never become system rules.

Runtime workspace tools bind home, task, attempt, and audience outside model-controlled arguments. Each operation rechecks that the task, attempt, route, and policy remain valid. No-Bash group Agents get the same scoped file operations without a general-purpose host path argument. Preset, project, and declared read-only reference directories keep their separate purposes. Old AgentHome notes, old memory skills, and harness auto-memory must not reintroduce archived knowledge. The Claude adapter stages filtered inherited/explicit skills, disables user setting sources, forces automatic memory and `CLAUDE.md` loading off, and injects committed preset rules explicitly. See the [runtime contract](agent-runtime-design-detail.md#agent-workspace-replaces-agenthome-knowledge).

Every attempt stages the current binary's managed Workspace skill and tools; it does not reinstall software inside the durable knowledge directory or reset existing notes. Inherited executor skills are resolved again, with retired memory and native/global knowledge writers excluded. Skills remain enabled when the managed Workspace skill or resolved skills are present. See [executable Claude initialization](agent-runtime-design-detail.md#executable-claude-initialization) for tool availability, task-local staging and preset updates.

## Storage and interfaces

`internal/agentworkspace` provides workspace listing, file listing, read, search, write, and history. The CLI entrypoint is `memgov agent workspace list/read/search/write/history`; `list` without `--workspace-id` lists workspaces and with it lists files; `read`/`history` use `--path`, `search` uses `--query` and `--limit`, and `write --input -` accepts `{path, content, expected_digest}`. Exact envelopes belong to the [CLI contract](../reference/cli-contract.md). The runtime installs `memgov-workspace` and uses the same file contract.

Writes require the current content digest (an empty digest means a new file), actor and request identity. A cross-process workspace lock serializes changes; the digest is checked under the lock, and content is replaced atomically. Conflicts require a fresh read and merge. Revisions preserve the previous digest, actor, request, time, and byte count in `<home>/agent-workspace-history/`; history reads return up to the latest 100 entries. History and archives are excluded from normal search.

Paths must remain inside the selected workspace and the permitted Markdown layout. Absolute paths, traversal, hidden/internal files, and symbolic links are rejected. Files, index, file count, and search work have explicit budgets in the package constants. An oversized index fails with a request to split it; it is not silently truncated.

The owner's loopback console exposes only GET operations:

| Path under `/api/v1` | Result |
| --- | --- |
| `/agent-workspaces` | Workspace identity and creation time |
| `/agent-workspaces/{id}/files` | Current file path, digest, bytes and modification time |
| `/agent-workspaces/{id}/files?query=...` | Up to 100 matches inside that workspace, with bounded snippets |
| `/agent-workspaces/{id}/file?path=...` | Current metadata and complete bounded Markdown content |
| `/agent-workspaces/{id}/history?path=...` | Revision metadata for that path |

The console rejects all workspace HTTP writes. Old memories and memory-workspaces APIs return not found. Its owner-facing list is not an Agent authorization endpoint; runtime tools resolve scope independently. Error mapping distinguishes invalid input, missing files, conflicts and internal failures. Markdown rendering never executes HTML or loads remote images. Console detail and history are loaded only when selected, and all responses are `no-store`.

## Destructive migration

Append Schema 27; retain historical migration files and their checksums. Every supported upgrade path must archive and verify a consistent complete old database plus the effective configuration and legacy AgentHome notes before destructive SQL. The upgrade archive contains a consistent `.db`, a `.knowledge.tar` of effective/default configuration and legacy AgentHome notes (including configured and applied homes), and a digest-bound manifest. Verification checks database integrity, foreign keys and archive digests. Archival failure prevents migration. `config migrate-workspaces` archives and converts the selected configuration before database upgrade; it does not apply runtime state. Configuration conversion removes obsolete memory/share/home/review fields while preserving preset, skills, routing and external-action permissions; normal parsing rejects unconverted legacy fields with a conversion instruction.

Drop old knowledge, candidate, review, publication, knowledge-version and dedicated review-job data. Keep internal Source/fragment/source-origin records because message queries, recall/retraction handling, retention and execution evidence still use them. Public source-ingestion/governance interfaces are removed; internal evidence is not a second long-term knowledge store.

Cancel unfinished old memory jobs and release their resources. Keep historical task metadata, but exclude legacy memory tasks from execution, delivery scans, new notices and dispatch so a restart cannot resend their retained results. Analysis summaries and direct-chat history also exclude those task results; operators can still inspect the historical records. Invalidate unsent drafts that depend on old Memory; preserve uncertain sending outcomes as `unknown`, without stripping references and resending. These changes and schema removal belong to the same database transaction. Do not import old notes into newly created workspaces. Archives remain outside all Agent knowledge mounts.

Rollback means restoring the verified pre-upgrade archive with the matching old binary. Reusing an old binary against the new schema is unsupported. A database-only backup does not back up workspace knowledge: recovery must separately preserve `<home>/agent-workspaces/` and `<home>/agent-workspace-history/` as well as `state.db` and effective configuration.

## Tests and remaining acceptance

- File persistence across reopen, Owner chat/background reuse and index refresh in an active session.
- Two groups sharing a preset still have separate knowledge; group requests cannot select Owner scope or modify shared reference inputs.
- Concurrent stale writes conflict rather than overwrite; cancelled/old attempts and invalid routes cannot perform new operations.
- Path traversal, symlink, internal metadata/history exposure, file-size and index budgets fail closed.
- Fresh installation, populated old-schema upgrade, archive failure, interrupted upgrade and unchanged migration checksums.
- Old tasks, pending drafts and uncertain sends retain safe terminal/recovery behavior; retained results and receipts cannot generate new legacy-task deliveries.
- Message intake, queries, retraction, retention, cancellation, confirmation, delivery and unknown-result recovery regressions.
- Frontend typecheck/build, workspace browser API and escaping tests, `make check`, runtime race tests and isolated offline acceptance.

These source checks use temporary homes and synthetic data. A separately authorized local upgrade and Owner private-chat persistence check are recorded in [implementation status](../implementation-status.md#authorized-local-acceptance), including the unresolved restart intake limitation. Live verification checks platform send status, application intake, completed task, workspace contents/history and the returned bot reply independently; a send receipt alone does not prove execution or knowledge retrieval. The full business loop, live background/group acceptance, root/child graph and proactive notification roadmap remain open.
