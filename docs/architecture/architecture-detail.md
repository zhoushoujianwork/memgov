# Architecture implementation details

The [main architecture](architecture.md) defines current responsibilities. The [Agent Workspace design](../design/agent-workspace-design.md) and [details](../design/agent-workspace-design-detail.md) define the destructive knowledge migration. This document is not an installation or live-platform acceptance record.

## Module responsibilities

| Module | Responsibility |
| --- | --- |
| `internal/cli` | CLI envelopes, configuration planning/applying, operational commands and workspace access |
| `internal/agentworkspace` | Workspace identity/layout, bounded files, scoped search, atomic writes and revision history |
| `internal/core` | Message/source evidence, tasks/attempts, authority, confirmations, operations, delivery and SQL migrations |
| `internal/channel` | Platform-specific intake, identity and transport adapters |
| `internal/runtime`, `internal/agent` | Analysis/execution, harness adaptation, scoped tools, context refresh, cancellation and recovery |
| `internal/sysprompt` | Shared identity/safety rules and ingress-specific instructions; these cannot expand capabilities |
| `internal/service` | Configured module supervision, shutdown, restart and macOS managed lifecycle |
| `internal/console`, `web/` | Owner-local observation, explicit service/task controls, safe read-only workspace browsing |
| `internal/observation`, `internal/runlog` | Process observation and bounded diagnostics |

Platform and harness adapters are independent boundaries. They do not own knowledge or permission truth. One service may host Owner and group entrypoints, but shared process and database access never imply shared audience authority.

## Persistence and context

Knowledge content is stored once in workspace Markdown files. SQLite owns messages, revisions, Source/fragment/source-origin evidence, runtime tasks and attempts, confirmations, delivery and operation metadata. File history is stored separately from current knowledge and excluded from search. Derived indexes, process heartbeats and logs are rebuildable or observational.

Runtime derives a workspace from verified `OwnerPrincipalID` or group `(channel_id, conversation_id)`, including when an Agent preset changes. Every execution receives the latest bounded `MEMORY.md` as context data. Topic files remain on demand. Task messages, quoted content and knowledge cannot enter trusted permission instructions. Legacy automatic memory loading, AgentHome notes, review jobs and old memory skills are removed from current knowledge input.

A workspace operation resolves home and audience from the active task/attempt and revalidates its authority. A no-Bash group can use scoped workspace read/write without getting arbitrary host paths. Shared reference directories are explicit and read-only; they do not redefine workspace identity. Full Bash is not process isolation and must not be described as such.

## Transactions, migration and recovery

SQL keeps operational transitions, expected versions, deduplication, confirmation and uncertain external results in transactions. Model calls and network requests run outside database write transactions. A Workspace write holds a bounded SQL write transaction while revalidating authorization and publishing the file under its cross-process lock, digest comparison and atomic replacement. The durable file is authoritative; SQLite operation metadata may lag if its commit fails. Read the current file before retrying against its digest. There is no single atomic SQL/filesystem transaction.

The schema migration is appended without rewriting previous migrations. Old knowledge is consistently archived and verified before deletion. Unfinished memory jobs are cancelled; old memory-dependent unsent drafts are invalidated; already-started uncertain sends remain `unknown`. Source evidence and message retention survive. New workspaces start empty and archives are not mounted to Agents.

Recovery must retain the appropriate executable, effective configuration, operational database, current workspace files and revision history. A SQLite backup alone cannot restore knowledge. Unknown side effects must be checked before replay. Policy changes, cancellation and stale attempts reject later results and tool calls.

## Product targets outside this refactor

The Personal Jarvis root/child graph, environment snapshots, coordinated sub-Agent summaries and automatic proactive result notifications are specified in the [Owner design](../design/owner-assistant-design.md). They are not claimed as delivered by the workspace migration. Current proactive completion is `record_only`; direct and group interactions retain their own delivery and confirmation behavior.

## Validation

Required cases cover fresh and old-schema databases; verified archive failure gates; Owner chat/background persistence; group separation under a shared preset; active-session index refresh; cancelled/stale access; concurrent writes; traversal and symlinks; explicit read-only references; source retention; confirmation; cancellation; and uncertain delivery recovery. Run source checks, runtime race tests, frontend build and isolated offline acceptance. Real harness execution, installation and real-platform delivery remain separately evidenced.
