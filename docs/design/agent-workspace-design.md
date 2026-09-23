# Agent Workspace knowledge

Status: implemented and validated in source on 2026-09-23; results and deployment boundaries are tracked in [implementation status](../implementation-status.md). This design does not claim that an installed service has been upgraded. [Implementation details](agent-workspace-design-detail.md) define the file contract and migration boundary.

## Goal and everyday use

An Agent keeps durable knowledge in readable files: preferences, project background, decisions, and reusable procedures. Ask the Agent to remember or correct something; it updates its own workspace and reads back the result before reporting success. Open the local console's **Workspaces** page to inspect files and their history.

Owner private chat and background work share one workspace for the verified Owner. Each group has its own workspace, even when several groups use the same Agent preset. Changing a preset does not move or merge knowledge. Conversation scratch directories, task outputs, and Git project worktrees remain separate.

## Core approach

- `MEMORY.md` is a short index; `notes/`, `projects/`, and `daily/` hold topic files with observation dates and source references.
- Every turn receives the latest bounded index. The Agent searches and reads detailed files only when needed, then maintains them directly without Candidate, Review, or Apply stages.
- Files are authoritative for knowledge. SQLite remains authoritative for messages, internal evidence, tasks, permissions, confirmations, delivery, and recovery.
- Workspace tools resolve the current task's audience in code. A group can maintain its own files without receiving general host filesystem access. Knowledge text cannot grant permissions.
- Shared references are explicit read-only directories. Owner files and old archives are never implicitly shared with groups. Full Bash still runs with the service account's permissions and requires its own operating-system boundary.

## Upgrade and current scope

This is a destructive replacement. Archive the complete old database, configuration, and AgentHome notes, verify the archive, then remove the old knowledge tables, APIs, extraction/review jobs, and card UI. New workspaces start empty; no old Memory or AgentHome knowledge is imported and no compatibility read or dual-write path remains.

The implementation includes current-file search, conflict-safe writes, file revision metadata, a runtime-managed `memgov-workspace` skill, CLI access, and a read-only console. Message retention, task controls, source evidence, confirmation, and uncertain delivery results keep their existing responsibilities.

Root/child task orchestration, automatic proactive completion notifications, new platform adapters, deployment, and real-platform acceptance are separate work. This change does not imply those goals are delivered.

## Implementation and acceptance order

Build file storage and its boundaries; connect all runtime entrypoints and tools; replace CLI and console surfaces; archive and migrate old state; synchronize documentation; run isolated checks and merge the completed branch locally.

Acceptance follows the [best-practice scenarios](../architecture/best-practice-scenarios.md): knowledge survives new sessions and restarts, Owner chat and background work reuse it, groups remain isolated, concurrent edits do not lose data, failed archival prevents destructive migration, and message/task/delivery regressions pass. Installed and live-platform results must be reported separately.
