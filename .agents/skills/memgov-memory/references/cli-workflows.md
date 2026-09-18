# CLI workflows

These examples use the current Source → Candidate → Memory workflow and assume `memgov` is on `PATH`. `MEMGOV_SCOPE` below is an example shell variable containing an existing workspace; the CLI's built-in workspace environment variable is `MEMGOV_WORKSPACE`. Global flags are shown explicitly so that scope and audit identity remain visible. Check `memgov version` and command help for the binary being used; design-only commands and legacy atom/card/battle commands are not part of these examples.

## Read and inspect

```bash
memgov --actor codex --workspace "$MEMGOV_SCOPE" doctor
memgov --actor codex --workspace "$MEMGOV_SCOPE" recall "release permissions" \
  --limit 8 --budget-chars 4000 --explain
memgov --actor codex --workspace "$MEMGOV_SCOPE" memory show MEMORY_ID

# Use source fallback when approved memory is absent or incomplete.
memgov --actor codex --workspace "$MEMGOV_SCOPE" search "release permissions" \
  --kind source --limit 8
memgov --actor codex --workspace "$MEMGOV_SCOPE" source show SOURCE_ID
```

Every normal result is a one-line JSON envelope. Read values from `data`; on failure, use `error.code` and `error.message`. Relevant exit codes are 2 for invalid input, 3 for a conflict, 4 for not found, 5 for unavailable or timeout, and 6 for denied.

## Ingest evidence

Prepare `source.json` with only material needed to support the claim:

```json
{
  "kind": "agent_result",
  "uri": "agent-note://codex/project-a/2026-09-14/release-check",
  "content": "In the test environment, the release completed after the permission check. Production was not tested.",
  "observed_at": "2026-09-14T08:00:00Z"
}
```

Then ingest it as an immutable source:

```bash
memgov --actor codex --workspace "$MEMGOV_SCOPE" \
  --idempotency-key "codex-release-check-20260914-v1" \
  source ingest --input source.json
```

Use an existing file path, task URL, or system identifier as the URI when one exists. Otherwise use a stable agent-note URI. Do not place secrets or sensitive content in the URI or idempotency key.

## Create a memory

Read the ingest response or run `source show`; populate `candidate-create.json` with the exact returned evidence identifiers. Use the selected fragment's `sha256`, not the whole-source hash:

```json
{
  "action": "create",
  "reason": "Preserve the verified test-environment release procedure and its limit.",
  "memory": {
    "category": "procedure",
    "title": "Test-environment release permission check",
    "summary": "Check release permissions before deployment; verified only in the test environment.",
    "content": "## Prerequisites\nUse the test environment and have release access.\n\n## Steps\nCheck permissions, then run the release.\n\n## Verification\nThe test-environment release completed. Production behavior is unknown.",
    "applicability": ["Verified only in the test environment"],
    "entities": ["release system"],
    "tags": ["release", "permissions"],
    "observed_at": "2026-09-14T08:00:00Z",
    "evidence": [
      {
        "source_id": "SOURCE_ID",
        "fragment_id": "FRAGMENT_ID",
        "sha256": "FRAGMENT_SHA256"
      }
    ]
  }
}
```

The selected `--workspace` fills `memory.workspace_id`; omit it from ordinary candidate input. Run the review sequence:

```bash
memgov --actor codex --workspace "$MEMGOV_SCOPE" \
  --idempotency-key "codex-release-memory-20260914-v1" \
  candidate submit --input candidate-create.json

memgov --actor codex --workspace "$MEMGOV_SCOPE" candidate validate CANDIDATE_ID
memgov --actor codex --workspace "$MEMGOV_SCOPE" candidate show CANDIDATE_ID
memgov --actor codex --workspace "$MEMGOV_SCOPE" candidate apply CANDIDATE_ID \
  --expected-digest CANDIDATE_DIGEST
memgov --actor codex --workspace "$MEMGOV_SCOPE" memory show MEMORY_ID
```

Read the complete `candidate show` result before copying its digest.

## Update a memory

Read the current memory first. A candidate update preserves the same memory ID while appending a version:

```json
{
  "action": "update",
  "target_id": "MEMORY_ID",
  "expected_version": 3,
  "reason": "New evidence expands the procedure to production.",
  "memory": {
    "category": "procedure",
    "title": "Release permission check",
    "summary": "Check release permissions before deployment in test and production.",
    "content": "## Prerequisites\nHave release access.\n\n## Steps\nCheck permissions, then run the release.\n\n## Verification\nThe procedure was verified in test and production.",
    "applicability": ["Test and production environments"],
    "evidence": [
      {
        "source_id": "NEW_SOURCE_ID",
        "fragment_id": "NEW_FRAGMENT_ID",
        "sha256": "NEW_FRAGMENT_SHA256"
      }
    ]
  }
}
```

Submit, validate, show, and then inspect the update before applying it:

```bash
memgov --actor codex --workspace "$MEMGOV_SCOPE" candidate diff CANDIDATE_ID
memgov --actor codex --workspace "$MEMGOV_SCOPE" candidate apply CANDIDATE_ID \
  --expected-digest CANDIDATE_DIGEST
```

If the target version changes, the command returns a conflict. Read the new current version, reassess the evidence and content, and submit a fresh candidate.

## Scope and mutation checklist

- Read scope: current workspace plus `global` by default.
- Write scope: exactly one explicit workspace.
- Cross-workspace search: add `--all-workspaces` only for an intentional cross-project task.
- Repeated write: reuse the same idempotency key only for byte-equivalent input and the same command.
- Lifecycle or destructive governance: use `retire`, `restore`, merge, purge, or backup restore only when the user requested that operation.
