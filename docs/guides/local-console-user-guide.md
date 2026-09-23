# Local console user guide

The console shows task records, Agent Workspace files, runtime observations and Agent configuration. The [main design](../design/local-console-design.md) defines scope. A built source tree does not mean your installed or running executable has been updated.

## Open the console

An existing unified service hosts the console at its configured loopback address, normally `http://127.0.0.1:8787/`. For a separate diagnostic instance:

```bash
memgov --home ~/.memgov --config ~/.memgov/config.yaml ui --open
```

Use `--port 0` if the normal port is occupied. Opening the console neither initializes nor upgrades the database and does not start collection or Agents. It needs no login token or cookie; it is limited to local owner access with Host/Origin checks. Stopping this diagnostic command does not stop a separately managed service.

The four pages are `/tasks`, `/workspaces`, `/running`, and `/settings`. Direct links, refresh, and browser back/forward navigation preserve the page. Filter and file selection are not encoded in the URL.

## Inspect knowledge

Open **Workspaces**, select an Owner or group workspace, then select a file. Search matches paths and current content in that workspace only. It does not add global notes or old versions. The right pane shows the full bounded Markdown body, time, size and digest; **View history** shows actor, request, time and revision digests. The console does not edit files.

`MEMORY.md` is the short index; detailed knowledge lives in `notes/`, `projects/`, and `daily/`. Ask the Agent to remember or correct a fact; a successful write is verified by reading it back. Owner private chat and background work share knowledge, while groups have separate files. Existing memory cards are archived during the [workspace upgrade](../design/agent-workspace-design.md) and are not imported into the new workspace.

## Inspect work and operation

Tasks show requests, attempts, available terminal output, results, artifacts and delivery records. Completion, platform acceptance and user acceptance are shown separately. Proactive work currently records completion locally; missing automatic notification is not a failed delivery. Explicit Agent communication remains separately auditable.

A running database state does not prove active execution; compare process observations and recent output. Sources that expire, are retracted or change revision invalidate affected task output. Unknown external results require receipt verification before replay. The console only offers **Continue task** when the service says it is safe.

The running page shows service and module state, pending work, source collection and coverage gaps. Settings separates current YAML declarations from applied runtime policies. Edit, preview and save a declaration, then use `config plan` and its returned versions/digest to apply it. Workspace identity is derived from the verified Owner or group route and is not a memory-scope field in the editor.

## Restart, versions and data lifecycle

The sidebar restart control reloads the installed executable; it does not build, install or apply YAML. It is disabled in an independent diagnostic console. The sidebar also separates the running build, installed build and latest comparable GitHub tag; a local match does not establish that the release is current.

Visible pages refresh roughly every three seconds. Background tabs and connection failures clear displayed content and reload it when active again. Message-source deadlines still clear affected task text immediately. Workspace notes have their own lifecycle and are not a raw-chat retention cache. The browser stores no note or chat bodies in localStorage/IndexedDB.

For frontend development, use a temporary service/data home and `make web-dev`. Build embedded assets with `npm --prefix web run build`. See [Web development](../../web/README.md). Source tests do not substitute for deployment and real-platform acceptance.
