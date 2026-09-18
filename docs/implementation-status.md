# Implementation status

This page describes the source tree. It does not claim that a particular local installation, DingTalk tenant, model account, or long-running service has been validated.

| Capability | Source status | Reproducible validation | Remaining boundary |
| --- | --- | --- | --- |
| Source, Candidate, Review, Memory, evidence, versioning, search, backup, and governance | Implemented | `make check` and the core package tests | Validate upgrades against a disposable copy before using production data |
| DingTalk application intake and DWS-backed collection | Implemented | Adapter, parser, identity, routing, and offline scenario tests | Requires the operator's own DingTalk configuration and live-platform acceptance |
| Owner private chat, group mention Agents, proactive processing, and permission policies | Implemented | Runtime, policy, confirmation, scheduling, and recovery tests | Model quality and platform delivery must be validated by each deployment |
| Local console and embedded React UI | Implemented | Type checking, frontend tests, Go console tests, and embedded build | The console is local-only and is not a multi-user authenticated service |
| macOS managed service | Implemented | LaunchAgent unit and opt-in isolated integration tests | Linux service management and desktop packaging are not included |
| Release archives for macOS and Linux, arm64 and amd64 | Implemented | `make release VERSION=<version>` | Signing, notarization, and Windows builds are not included |
| End-to-end operational acceptance | Environment-specific | Offline fixtures are included | Real credentials, conversations, logs, and acceptance evidence stay outside the public repository |

The current implementation still prioritizes a single local SQLite authority. Tool count, Agent count, and process liveness are not acceptance criteria; the relevant outcomes are safe scope, useful task completion, verifiable delivery, and governed memory reuse.

See the [roadmap](roadmap.md) for planned work and the [documentation index](README.md) for detailed designs and guides.
