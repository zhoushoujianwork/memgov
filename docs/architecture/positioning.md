# Product positioning: a local Personal Jarvis

Status: product direction with current storage decisions. The [implementation status](../implementation-status.md) separates shipped source, planned capabilities and deployment acceptance.

memgov is an open-source personal work assistant running in the user's own environment. Communication platforms and Agent harnesses are adapter boundaries, rather than the product identity. DingTalk/DWS and Claude are current integrations; mentioning other platforms or harnesses does not mean those adapters ship.

The product goal is to connect work discovery, background context, authorized execution, verifiable delivery and reusable experience. A personal Agent and group-specific Agents retain distinct identities and audiences. Tool count and Agent count are not measures of success.

## Persistent knowledge and operational state

[Agent Workspaces](../design/agent-workspace-design.md) hold readable, editable Markdown knowledge. Owner private chat and proactive work share the verified Owner's workspace. Groups are isolated by channel and conversation, regardless of which preset they use. The short index is refreshed every turn and detailed notes are read on demand.

SQLite holds messages, internal evidence, temporary task progress, permissions, delivery and recovery facts. It is the authority for those operational records, while workspace files are the authority for knowledge. The old Candidate/Review/Memory governance model is archived and removed; internal Source evidence remains for message behavior.

## Interfaces and boundaries

Daily work enters through configured chat or proactive processing. CLI and the local console configure, diagnose and inspect the system. The runtime-managed `memgov-workspace` skill explains the scoped knowledge tools; it does not grant shell, messaging or production permissions. Knowledge text cannot authorize an action or disclosure.

Only explicit read-only reference directories are shared. Owner knowledge is not automatically mounted into groups. Full Bash continues to run as the service account and is not an operating-system sandbox.

<a id="检索定位"></a>
## Retrieval choice

Use a short `MEMORY.md` index plus focused file search and reads. Keep project decisions and procedures together, record observation dates and source references, and correct stale knowledge with a new version. Do not load an entire note archive every turn or copy raw chat indiscriminately. Current-file search is scoped to one workspace; historical revisions and migration archives are excluded.

## Next product milestones

Proactive completion currently records results locally (`record_only`). Root/child task orchestration and automatic result notifications remain future work. First validate one real work loop and the workspace migration, then add these capabilities with explicit scope and evidence. See the [architecture](architecture.md), [acceptance scenarios](best-practice-scenarios.md) and [roadmap](../roadmap.md).
