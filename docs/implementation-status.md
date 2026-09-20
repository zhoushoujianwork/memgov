# Implementation status

This page describes the source tree and the Owner Assistant product line. It does not claim that a particular local installation, DingTalk tenant, model account, or long-running service has been validated. Group-mounted Jarvis remains an independent ingress and keeps its configured capabilities while the Owner Assistant work is rolled out.

| Capability | Source status | Reproducible validation | Remaining boundary |
| --- | --- | --- | --- |
| Source, Candidate, Review, Memory, evidence, versioning, search, backup, and governance | Implemented | `make check` and the core package tests | Validate upgrades against a disposable copy before using production data |
| DingTalk application intake and DWS-backed collection | Implemented | Adapter, parser, identity, routing, and offline scenario tests | Requires the operator's own DingTalk configuration and live-platform acceptance |
| Owner private chat, group-mounted Jarvis, proactive processing, and permission policies | Partially implemented / compatibility maintained | Runtime, policy, confirmation, scheduling, recovery, and group-route regression tests | Owner root/child task graph, environment snapshot, proactive result notification, and live platform delivery must be verified by deployment; existing group Jarvis capability is intentionally preserved |
| Single active `~/.memgov/config.yaml` and runtime plan diagnostics | Implemented in source | Config validation/plan and migration regression tests | A deployed host must stop old dependent runtimes, apply the merged declaration, restart the unified service, and archive `config.dual.yaml` |
| `memgov-memory` cross-Agent governance protocol | Documented and skill surfaces aligned | Skill contract review plus CLI/core governance tests | Stable external Agent/API packaging and live multi-Agent acceptance remain deployment work |
| Local console and embedded React UI | Implemented | Type checking, frontend tests, Go console tests, and embedded build | The console is local-only and is not a multi-user authenticated service |
| macOS managed service | Implemented | LaunchAgent unit and opt-in isolated integration tests | Linux service management and desktop packaging are not included |
| Release archives for macOS and Linux, arm64 and amd64 | Implemented | `make release VERSION=<version>` | Signing, notarization, and Windows builds are not included |
| End-to-end operational acceptance | Environment-specific | Offline fixtures are included | Real credentials, conversations, logs, and acceptance evidence stay outside the public repository |

The current implementation still prioritizes a single local SQLite authority. Tool count, Agent count, and process liveness are not acceptance criteria; the relevant outcomes are safe scope, useful task completion, verifiable delivery, and governed memory reuse.

See the [roadmap](roadmap.md) for planned work and the [documentation index](README.md) for detailed designs and guides.
