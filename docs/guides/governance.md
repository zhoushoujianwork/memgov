# Workspace knowledge, archival and recovery

The current knowledge model is [Agent Workspace](../design/agent-workspace-design.md). Candidate/Review/Apply, memory merging, published memory scopes, retirement and purge commands are removed. The operational database still governs messages, tasks, permissions, confirmation and uncertain external effects.

## Maintain knowledge

Agents maintain their own `MEMORY.md`, `notes/`, `projects/` and `daily/` files. Keep the index short. Record dates, source references and conditions alongside durable conclusions; keep temporary progress in task records. For a correction, read the file, write against its current digest, and read back the result. A stale digest requires rereading and merging rather than force-overwriting.

Workspace tools bind runtime task and audience. A group cannot switch to Owner scope by naming a path or claiming permission in a note. Shared references are explicit read-only inputs. The local owner can inspect files and revision metadata through the console or the workspace CLI; exact flags are in the [CLI contract](../reference/cli-contract.md).

## Upgrade from database memory

1. Stop the old service before converting its effective configuration.
2. Run `memgov --home /path/to/data --config /path/to/config.yaml config migrate-workspaces`. This archives the original configuration and AgentHome notes, removes legacy memory/share fields, validates the result, and does not apply database runtime configuration.
3. Run `memgov --home /path/to/data --config /path/to/config.yaml init` with the new executable. The supported schema upgrade makes and verifies a complete old `.db` plus `.knowledge.tar` (configuration and legacy AgentHome notes), bound by a digest manifest, before deleting the old knowledge model. Managed-service upgrades use the same archive-before-drop gate after configuration conversion.
4. Check configuration and schema status before starting the new executable. Review its `config plan` before applying runtime declarations. New workspaces start empty; old notes are not imported.

A failed archive or unsupported schema blocks destructive migration. Keep old archives outside Agent directories. Never run the old executable against the upgraded database.

## Back up and restore

A complete recovery set includes `state.db`, the effective configuration, `<home>/agent-workspaces/`, `<home>/agent-workspace-history/`, and any separately managed secrets or preset repositories. The normal `backup` commands protect operational SQLite state only; separately copy those knowledge and history directories for knowledge recovery. Stop writes while capturing a filesystem recovery set; copying an active `state.db` alone is not a consistent database backup.

Use the supported database backup/verify commands described by the installed executable. Restore with a matching schema-compatible binary. To undo the destructive migration, restore its verified old archive and use the corresponding old executable; do not merge archived Memory entries back into current workspaces automatically.

Raw-message retention and retraction remain separate from note retention. Do not copy whole chat histories into notes to bypass the raw-source window. External exports, copied files and old archives require their own handling; database cleanup cannot claim to erase these copies.
