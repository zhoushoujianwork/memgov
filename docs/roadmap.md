# Implementation roadmap

The [Personal Jarvis design](design/owner-assistant-design.md) defines the product goal; the [best-practice scenarios](architecture/best-practice-scenarios.md) define useful outcomes. Current evidence is recorded in [implementation status](implementation-status.md). Planned capabilities must not be presented as installed behavior.

## Workspace replacement and next acceptance

The [Agent Workspace refactor](design/agent-workspace-design.md) replaces the database memory pipeline, archives old state before migration, creates empty knowledge workspaces, updates runtime entrypoints and replaces the card console. Development and isolated checks completed in a dedicated worktree, followed by one local merge after the final gates. A separately authorized local service upgrade and Owner private-chat persistence check then passed. No push or release was performed.

| Order | Work | Status and completion condition |
| --- | --- | --- |
| 1 | File storage, identity and tool boundaries | Implemented and offline-validated: Owner reuse, group isolation, stale-access denial and conflict-safe cross-process writes |
| 2 | Runtime, CLI and console entrypoints | Implemented and offline-validated: latest bounded index, scoped retrieval, managed skill and read-only file browser; retired knowledge entrypoints removed |
| 3 | Upgrade and regression verification | Implemented and offline-validated: verified archives, explicit config conversion, transactional migration, retained evidence and safe uncertain-send recovery |
| 4 | Source validation and local delivery | All source gates passed on 2026-09-23. Delivery uses a reviewed implementation commit and one local merge commit; Git records the exact revisions |
| 5 | Authorized local upgrade and Owner private-chat persistence | Passed on 2026-09-23: verified archival, installed/running build identity, Schema 27, note write, `/clear`, and retrieval/update after restart; restart intake readiness remains a known limitation |
| 6 | One real business work loop | Next acceptance: demonstrate discover → investigate → execute → accept → write knowledge → reuse with scoped real evidence; live background reuse and group isolation remain unverified |

## Next product work

- Executable Claude tool parity and current skill initialization have passed source and isolated real-Claude checks. Complete the separately authorized conversation configuration and one real group business loop; missing robot callbacks and DWS Stream timestamp normalization remain independent follow-ups.
- Investigate the request accepted by DWS without observed application intake immediately after restart; establish receiver readiness and the delivery guarantee before claiming lossless restart. Continue investigating other runtime failures independently of storage format.
- Implement and verify Personal root/child task relationships and environment snapshots before advertising coordinated sub-Agent work.
- Decide and implement automatic proactive result/blocked/confirmation notifications with deduplication and receipt recovery. Current completion remains `record_only` until that work is delivered.
- Continue group-message, permission and original-group reply regressions. Workspace separation replaces old published-memory sharing without granting Owner private history to groups.
- Validate prompt safety with real models in an isolated environment. Full Bash requires a separate operating-system isolation decision; prompt checks do not provide that boundary.
- Add new platform/harness adapters, Desktop packaging and broader collaboration when supported by real tasks and acceptance evidence.

The old memory-list, hotword-ingestion, published-memory sharing and external Candidate/Review/Apply backlog is superseded by the Workspace design. Historical documents remain available for understanding previous versions, not as current command guidance. Each follow-up updates its main/detailed design, status and this roadmap before its final commit.
