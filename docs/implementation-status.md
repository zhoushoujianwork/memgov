# Implementation status

This page describes the source tree, not a particular installed executable or live service. Agent Workspace files replace the database memory model. Full functional checks, the race gate, offline acceptance and the frontend build passed on 2026-09-23.

| Capability | Source status | Validation and remaining boundary |
| --- | --- | --- |
| Agent Workspace files, identity, scoped read/search/write and history | Implemented and offline-validated | Owner chat/background reuse, active-session index refresh, reopen persistence, cross-process conflict handling, path budgets and group isolation pass |
| Destructive old-memory archival and Schema 27 upgrade | Implemented and offline-validated | Fresh install and old-schema init/service upgrades pass; archive failure and transaction interruption preserve old state; legacy tasks are fenced and unknown sends retained |
| Workspace CLI and runtime-managed skill | Implemented and offline-validated | No-Bash scoped access, stale task/attempt/route rejection, config conversion and legacy field/skill/API removal pass |
| Local workspace browser | Implemented and package-validated on 2026-09-23 | Console package tests, frontend typecheck/build and UI/Markdown/date tests pass; no real installation or platform claim |
| DingTalk application intake and DWS collection | Retained and regression-tested | Parser, identity, routing, retraction, source retention and delivery tests use synthetic fixtures; Scenario 1 passes all ten offline cases |
| Runtime policy, confirmation, cancellation and recovery | Retained and regression-tested | Full functional and race checks pass, including cancellation, confirmation, delivery and unknown-result recovery |
| Proactive completion | `record_only` remains current | Independent authorized communication is separate; automatic completion notifications remain future work |
| Personal root/child task graph and broad environment snapshots | Product design, not delivered by this refactor | Require implementation plus separate offline and business acceptance |
| Platform/harness adapters | Existing contracts and Claude implementation retained | Each additional adapter needs its own integration and live acceptance |
| Single active config, macOS managed service and local operations | Existing source capabilities retained | Legacy configuration conversion precedes upgrade; no running service is replaced by this development task |
| Release archives and Desktop | Existing release packaging; Desktop remains future work | No push, release, installation or deployment is part of this refactor |

Workspace files are authoritative for durable knowledge. SQLite is authoritative for operational state and internal Source evidence. Candidate, Review, Memory, old publication relations and the memory-card UI are superseded. Historical migration files remain intact so old databases can be recognized and archived safely.

The completed delivery checks are `make check`, `make test-runtime-race`, `scripts/runtime-offline-acceptance.sh`, and `npm --prefix web run build`. The race target allows 30 minutes for instrumented SQLite migration fixtures; the core suite completed in about ten minutes. Documentation review checks the staged content before the implementation commit and again before the local merge. These checks do not establish live-platform acceptance or upgrade an installed service.

The relevant best-practice scenarios are Owner private work, proactive discovery and isolated group work, including experience reuse. The listed behaviors are verified offline; a real discover → investigate → execute → accept → write knowledge → reuse loop still needs separately authorized deployment and real evidence. Personal root/child orchestration and proactive completion notifications remain future work.

Historical source or local installation tests recorded in individual designs describe their original versions and were not automatically rerun for this refactor. Real credentials, messages and acceptance evidence stay outside the public repository. See the [roadmap](roadmap.md), [workspace design](design/agent-workspace-design.md) and [documentation index](README.md).
