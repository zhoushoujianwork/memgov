# Implementation roadmap

The [Personal Jarvis design](design/owner-assistant-design.md) defines the product goal; the [best-practice scenarios](architecture/best-practice-scenarios.md) define useful outcomes. Current evidence is recorded in [implementation status](implementation-status.md). Planned capabilities must not be presented as installed behavior.

## Workspace replacement and next acceptance

The [Agent Workspace refactor](design/agent-workspace-design.md) replaces the database memory pipeline, archives old state before migration, creates empty knowledge workspaces, updates runtime entrypoints and replaces the card console. Development and isolated checks run in a dedicated worktree, followed by one local merge after the final gates. This delivery does not push, release, replace the installed binary or restart a real service.

| Order | Work | Status and completion condition |
| --- | --- | --- |
| 1 | File storage, identity and tool boundaries | Implemented and offline-validated: Owner reuse, group isolation, stale-access denial and conflict-safe cross-process writes |
| 2 | Runtime, CLI and console entrypoints | Implemented and offline-validated: latest bounded index, scoped retrieval, managed skill and read-only file browser; retired knowledge entrypoints removed |
| 3 | Upgrade and regression verification | Implemented and offline-validated: verified archives, explicit config conversion, transactional migration, retained evidence and safe uncertain-send recovery |
| 4 | Source validation and local delivery | All source gates passed on 2026-09-23. Delivery uses a reviewed implementation commit and one local merge commit; Git records the exact revisions |
| 5 | Separately authorized deployment and one real work loop | Next acceptance: verify installed/running build identity, rehearse backup and rollback, then demonstrate discover → investigate → execute → accept → write knowledge → reuse with scoped real evidence |

## Next product work

- Investigate current runtime failures independently of storage format; replacing memory does not by itself fix permissions, task conflicts or timeouts.
- Implement and verify Personal root/child task relationships and environment snapshots before advertising coordinated sub-Agent work.
- Decide and implement automatic proactive result/blocked/confirmation notifications with deduplication and receipt recovery. Current completion remains `record_only` until that work is delivered.
- Continue group-message, permission and original-group reply regressions. Workspace separation replaces old published-memory sharing without granting Owner private history to groups.
- Validate prompt safety with real models in an isolated environment. Full Bash requires a separate operating-system isolation decision; prompt checks do not provide that boundary.
- Add new platform/harness adapters, Desktop packaging and broader collaboration when supported by real tasks and acceptance evidence.

The old memory-list, hotword-ingestion, published-memory sharing and external Candidate/Review/Apply backlog is superseded by the Workspace design. Historical documents remain available for understanding previous versions, not as current command guidance. Each follow-up updates its main/detailed design, status and this roadmap before its final commit.
