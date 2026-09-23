# Implementation status

This page separates source validation from a limited, separately authorized live check. Agent Workspace files replace the database memory model. Full functional checks, the race gate, offline acceptance and the frontend build passed on 2026-09-23; one local macOS installation was then upgraded and tested through Owner private chat.

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
| Single active config, macOS managed service and local operations | Source-validated; one authorized local upgrade observed | Configuration conversion, verified archival, Schema 27 upgrade, healthy doctor result and managed restart passed on 2026-09-23 |
| Release archives and Desktop | Existing release packaging; Desktop remains future work | Local source delivery and one authorized installation are complete; no push or release was performed |

Workspace files are authoritative for durable knowledge. SQLite is authoritative for operational state and internal Source evidence. Candidate, Review, Memory, old publication relations and the memory-card UI are superseded. Historical migration files remain intact so old databases can be recognized and archived safely.

The completed delivery checks are `make check`, `make test-runtime-race`, `scripts/runtime-offline-acceptance.sh`, and `npm --prefix web run build`. The race target allows 30 minutes for instrumented SQLite migration fixtures; the core suite completed in about ten minutes. Documentation review checks the staged content before the implementation commit and again before the local merge. These source checks are separate from the limited live acceptance below.

## Authorized local acceptance

On 2026-09-23, one macOS service was upgraded from Schema 26 to Schema 27 using the local build `2.0.0-rc1+workspace.6276248`, after configuration conversion and archive verification. The installed build and restarted process were checked, `doctor` reported healthy, and pre-existing uncertain delivery records remained `unknown`.

A real DWS Owner-to-bot private exchange verified writing a workspace note and index, starting a new conversation with `/clear`, and reading and updating the note after a service restart. The final bot reply returned the stored test value even though it was absent from the new request and index; file contents and revision history were checked separately.

One request sent immediately after the service reported running was accepted by DWS but had no observed application intake. A later distinct probe was received and completed. The cause remains unproven: supervisor state alone is insufficient evidence of bot Stream readiness, and application intake has no history backfill. This acceptance does not establish lossless delivery across restart.

The relevant best-practice scenarios are Owner private work, proactive discovery and isolated group work, including experience reuse. The live check covers Owner private knowledge persistence only. Owner/background reuse, group isolation and broader regressions remain verified offline; a real discover → investigate → execute → accept → write knowledge → reuse business loop still needs scoped live evidence. Personal root/child orchestration and proactive completion notifications remain future work.

Historical source or local installation tests recorded in individual designs describe their original versions and were not automatically rerun for this refactor. Real credentials, messages and acceptance evidence stay outside the public repository. See the [roadmap](roadmap.md), [workspace design](design/agent-workspace-design.md) and [documentation index](README.md).
