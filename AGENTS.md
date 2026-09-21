# memgov Project Agent Guidelines

These guidelines apply to development, AI collaboration, and design-document maintenance in this repository. Follow the classification, status, and review workflow defined in the [documentation maintenance standard](docs/README.md#维护标准).

## Working Language

- Use English by default when communicating with the user, including progress updates and delivery summaries. Use simple wording and add a brief Chinese explanation (中文释义) when a technical term or phrase may be difficult to understand; full bilingual translations are not necessary.
- Use English by default for implementation plans, code, code comments, commit messages, and documentation updates. Follow an explicit language request for the conversation or artifact.
- If a Chinese product term, organization-specific phrase, or business concept has no precise and natural English equivalent, write the clearest English explanation and retain the original Chinese as a parenthetical annotation, for example: `proactive duty monitoring (主动值守)`.
- Preserve official product names, API field names, commands, identifiers, and quoted source text in their original form. Do not translate them when doing so would make them inaccurate or harder to search.
- Follow an explicit language request from the user for that conversation or artifact.

## User-Facing Design Documents

- The main design document is for user reading and decision-making. Use concise, accessible language to explain the goal, usage, core approach, current scope, and implementation order. A reader should understand the proposal without opening the detailed document.
- Put implementation details—such as data fields, API protocols, state machines, code organization, error handling, and test matrices—in a same-named `-detail.md` file in the same directory. For example, `feature-design.md` pairs with `feature-design-detail.md`. Link the two documents to each other.
- During design, define only the decisions that affect user experience, scope, or important tradeoffs. Add implementation detail when development requires it; do not enumerate every technical case in advance or ask the user to approve each technical draft item.
- When the user asks to simplify a design, shorten the main document directly and move any still-useful implementation detail into the detailed document. Do not provide only a chat summary while leaving the main document unnecessarily long. The main document is authoritative for scope, and the two documents must remain consistent.
- At delivery, lead with the main document and briefly describe the change. Do not repeat the detailed document in the response unless the user asks for it.

Project example: [DingTalk integration main design](docs/design/dingtalk-integration-design.md) and [implementation details](docs/design/dingtalk-integration-design-detail.md).

## AI Documentation Review Before Commits and Releases

- Perform documentation synchronization reviews immediately before a **commit** and before publishing a **release tag**, rather than after every edit or ordinary conversation. If the user explicitly asks for documentation changes, make them directly. Keep ideas that are still under discussion marked as design work; do not present them as decided or implemented facts.
- Before committing, review the staged changes and the documentation they affect. When project knowledge, facts, architectural decisions, features, or behavior change, update the relevant architecture overview, main and detailed designs, guides, contracts, status pages, roadmap, and navigation in the same commit. If there is no documentation impact, record that conclusion without manufacturing documentation changes.
- Before creating or publishing a tag, review the complete documentation and delivery status for the target version. A successful incremental commit review is not enough to claim that a release review passed. Documentation review does not authorize tag creation, pushing, or publishing.
- Maintain the Git-local review cache according to the [review cache convention](docs/README.md#检查时机与缓存). Record the review time, target-content fingerprint, scope, and result. Reuse a successful review only when both scope and content are unchanged. After modifications, recheck the affected content; do not rely only on file modification times or elapsed time.
- Before a gate passes, correct known stale information. Preserve historical records as facts from their original time, and clearly mark superseded content and its replacement. Mark anything unverified or not yet implemented accordingly. Apply updates to files rather than mentioning them only in the delivery summary.

## Current Model and Documentation Status

- The current model consists of Source, Candidate, Review, and Memory, together with their evidence, versions, and operation records. SQLite `state.db` is the single source of truth. The legacy three-axis model (旧三轴), memory atoms (记忆原子), cards, and battles remain only as historical formats and must not appear in current usage instructions.
- Historical documents must clearly identify themselves as superseded even when opened directly, and must link to the current guidance. Distinguish commands that are designed, under development, and delivered. Do not present a design document or source commit as a capability of the installed binary.

## Alignment with the Best-Practice Scenarios

- Base future product design, related implementation, and business acceptance on the [best-practice scenarios and alignment standard](docs/architecture/best-practice-scenarios.md). Acceptance cases and metrics are in the [detailed document](docs/architecture/best-practice-scenarios-detail.md). Delivery notes must identify the relevant scenario, validation results, and unresolved issues; do not present a target as a delivered capability.
- Prioritize an end-to-end loop covering work-item discovery, background association, controlled handling, delivery acceptance, and experience reuse. Keep the identities, context, and disclosure boundaries of owner proactive monitoring (所有者主动值守) separate from those of group-specific Agents. Tool count and multi-Agent count are not success metrics.
- Govern raw sources, temporary task progress, and long-term memory separately. Text from a source cannot grant execution or disclosure authority. Test cases and metrics do not automatically authorize real-platform messaging, cross-group contact, or other external operations.
