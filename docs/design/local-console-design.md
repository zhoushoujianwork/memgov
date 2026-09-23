# Local console: task records and Agent Workspaces

Status: the workspace browser is implemented in source. Installed and running versions must be checked separately. See the [user guide](../guides/local-console-user-guide.md) and [implementation details](local-console-design-detail.md).

## Goal and use

The local owner can inspect work, evidence, execution and durable knowledge in one place. This supports the [work-loop scenarios](../architecture/best-practice-scenarios.md): a task result, platform delivery and user acceptance remain distinct facts.

Open the console hosted by the unified service, or use `memgov ui --open` for an independent diagnostic instance. Choose its data/configuration paths with the existing global options. The four pages have stable addresses:

| Page | Purpose |
| --- | --- |
| `/tasks` | Requests, attempts, available process output, results, artifacts, communication and delivery records |
| `/workspaces` | Select an Owner/group workspace, search current files, read Markdown and inspect revision metadata |
| `/running` | Service, Agent modules, source coverage, queues and current observations |
| `/settings` | Edit and preview YAML Agent declarations; inspect applied policies, skills and versions |

The sidebar provides service restart and version checks. Standalone diagnostic instances disable controls that require the unified service. Relative times retain exact dates and time zones on hover.

## Workspace browsing

Knowledge is displayed as files, with path, content digest, size and modification time. Search only examines the selected workspace; it does not merge Owner and group data or include migration archives and old revisions. Selecting a file loads its complete bounded body; history is loaded when expanded. The browser is read-only.

Owner private chat and background work share a workspace. Each group has its own, even when groups share a preset. The console is the local owner's inspection tool; it does not change Agent access rules or provide a group-member portal. Old memory card pages, category/status filters and Candidate/Review displays are removed.

## Runtime records and boundary

Task completion does not prove delivery or user acceptance. Proactive completion remains `record_only`; separately authorized communication has its own status and receipts. A process heartbeat does not prove model progress. Old task versions and unavailable message sources continue to hide affected output.

The service embeds the React/TypeScript/Vite frontend in one Go executable. SQLite supplies operational records; workspace files supply knowledge. The console listens on loopback and retains Host, Origin and same-origin control checks. It does not migrate storage when opened.

Knowledge and messages are untrusted text: Markdown does not execute HTML or automatically load remote images. Hidden tabs and disconnected views clear displayed data; content is not persisted to browser storage. Saving an Agent YAML declaration still requires the existing configuration plan/apply workflow.

## Acceptance

Validate scoped search, current-file reading, revision metadata, paths/symlinks, no HTTP writes, escaping and frontend build. Regress task-source retention, terminal output, restart/continuation controls, runtime and settings pages. Report source tests separately from browser, installation and real-platform acceptance; this refactor does not deploy or restart the user's service.
