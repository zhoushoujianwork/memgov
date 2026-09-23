import type {
  AgentWorkspace,
  WorkspaceFile,
  WorkspaceDocument,
  WorkspaceRevision,
} from "../types";
import { Empty, Heading, KV, Time } from "../components/common";
import { Markdown } from "../components/Adapters";

export interface WorkspaceFilters {
  workspace: string;
  query: string;
}
export function workspacePath(
  id: string,
  endpoint: "files" | "file" | "history",
  value = "",
) {
  const params = new URLSearchParams(
    endpoint === "files" ? { query: value } : { path: value },
  );
  return `agent-workspaces/${encodeURIComponent(id)}/${endpoint}?${params}`;
}
export function workspaceLabel(workspace: AgentWorkspace) {
  return workspace.kind === "owner"
    ? `Owner · ${workspace.owner_principal_id}`
    : `群 · ${workspace.channel_id} / ${workspace.conversation_id}`;
}
export function Workspaces({
  filters,
  workspaces,
  files,
  selected,
  detail,
  history,
  showHistory,
  onFilter,
  onSelect,
  onHistory,
  onRefresh,
}: {
  filters: WorkspaceFilters;
  workspaces: AgentWorkspace[];
  files: WorkspaceFile[];
  selected: string;
  detail?: WorkspaceDocument;
  history?: WorkspaceRevision[];
  showHistory: boolean;
  onFilter: (filters: WorkspaceFilters) => void;
  onSelect: (path: string) => void;
  onHistory: () => void;
  onRefresh: () => void;
}) {
  const workspace = workspaces.find((item) => item.id === filters.workspace);
  return (
    <>
      <Heading title="Agent Workspace">
        查看 Agent 持续整理的知识文件与修改历史。Owner
        私聊和后台值守共用工作区，各群独立。
      </Heading>
      <div className="filters workspace-filters">
        <select
          aria-label="选择工作区"
          value={filters.workspace}
          onChange={(event) =>
            onFilter({ ...filters, workspace: event.target.value })
          }
        >
          <option value="">选择工作区</option>
          {workspaces.map((item) => (
            <option key={item.id} value={item.id}>
              {workspaceLabel(item)}
            </option>
          ))}
        </select>
        <input
          type="search"
          aria-label="搜索当前工作区"
          placeholder="搜索路径和正文"
          value={filters.query}
          disabled={!workspace}
          onChange={(event) =>
            onFilter({ ...filters, query: event.target.value })
          }
        />
        <button className="button" onClick={onRefresh}>
          刷新
        </button>
      </div>
      {!workspaces.length ? (
        <Empty>暂无工作区。Agent 首次使用时会创建自己的知识文件。</Empty>
      ) : !workspace ? (
        <Empty>选择一个工作区，查看其文件。</Empty>
      ) : (
        <>
          <p className="workspace-context">
            {workspaceLabel(workspace)} · 只读浏览 · {files.length} 个文件
            {filters.query && "（最多显示 100 条匹配）"}
          </p>
          <div className="workspace-browser">
            <div className="task-list" aria-label="知识文件">
              {files.map((file) => (
                <button
                  key={file.path}
                  className={`task-row ${selected === file.path ? "selected" : ""}`}
                  aria-pressed={selected === file.path}
                  onClick={() => onSelect(file.path)}
                >
                  <strong>{file.path}</strong>
                  {file.snippet && <p className="preview">{file.snippet}</p>}
                  <div className="row-foot">
                    <span>{file.bytes} bytes</span>
                    <Time value={file.updated_at} />
                  </div>
                </button>
              ))}
              {!files.length && (
                <Empty>
                  {filters.query ? "没有匹配的文件。" : "工作区暂无知识文件。"}
                </Empty>
              )}
            </div>
            <article
              className="detail-panel workspace-document"
              aria-label="文件正文"
            >
              {!selected ? (
                <Empty>选择文件，查看正文和修改历史。</Empty>
              ) : !detail ? (
                <Empty>正在读取文件…</Empty>
              ) : (
                <>
                  <h2>{detail.path}</h2>
                  <KV label="最近修改">
                    <Time value={detail.updated_at} />
                  </KV>
                  <KV label="大小">{detail.bytes} bytes</KV>
                  <KV label="内容摘要">
                    <code>{detail.digest}</code>
                  </KV>
                  <div className="detail-section">
                    <Markdown content={detail.content} />
                  </div>
                  <div className="detail-section">
                    <button
                      className="button"
                      aria-expanded={showHistory}
                      onClick={onHistory}
                    >
                      {showHistory ? "收起修改历史" : "查看修改历史"}
                    </button>
                    {showHistory &&
                      (history === undefined ? (
                        <p className="muted">正在读取历史…</p>
                      ) : !history.length ? (
                        <p className="muted">暂无修改记录。</p>
                      ) : (
                        history.map((revision) => (
                          <div
                            className="workspace-revision"
                            key={`${revision.digest}-${revision.request_id}`}
                          >
                            <KV label="修改时间">
                              <Time value={revision.created_at} />
                            </KV>
                            <KV label="修改者">{revision.actor}</KV>
                            <KV label="请求">{revision.request_id}</KV>
                            <KV label="内容摘要">
                              <code>{revision.digest}</code>
                            </KV>
                            {revision.previous_digest && (
                              <KV label="上一版摘要">
                                <code>{revision.previous_digest}</code>
                              </KV>
                            )}
                            <KV label="大小">{revision.bytes} bytes</KV>
                          </div>
                        ))
                      ))}
                  </div>
                </>
              )}
            </article>
          </div>
        </>
      )}
    </>
  );
}
