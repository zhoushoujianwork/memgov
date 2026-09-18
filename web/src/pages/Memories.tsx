import type {
  MemoryCard,
  MemoryList,
  MemoryDetail,
  MemoryHistory,
  MemorySources,
  Workspace,
  Evidence,
} from "../types";
import { CardDetailView } from "../components/CardDetailView";
import {
  MemoryCollectibleCard,
  categories,
  statuses,
} from "../components/MemoryCollectibleCard";
import { Markdown } from "../components/Adapters";
import { Empty, Heading, Time } from "../components/common";
export interface MemoryFilters {
  q: string;
  workspace: string;
  category: string;
  status: string;
  page: number;
}
export function memoryParams(filters: MemoryFilters) {
  return new URLSearchParams({
    ...filters,
    page: String(filters.page),
    workspace: filters.workspace === "*" ? "global" : filters.workspace,
    all_workspaces: String(filters.workspace === "*"),
  });
}
export function memoryPath(card: MemoryCard, suffix = "") {
  return `memories/${encodeURIComponent(card.id)}${suffix}?${new URLSearchParams({ workspace: card.workspace_id || "global" })}`;
}
function EvidenceView({ evidence }: { evidence?: Evidence[] }) {
  return (
    <>
      {evidence?.map((e, i) => (
        <div className="memory-evidence" key={i}>
          <code className="id">
            {e.source_id} / {e.fragment_id}
          </code>
          {e.quote && <blockquote>{e.quote}</blockquote>}
        </div>
      ))}
    </>
  );
}
export function Memories({
  filters,
  list,
  workspaces,
  selected,
  detail,
  sources,
  history,
  showSources,
  showHistory,
  onFilter,
  onSelect,
  onClose,
  onSources,
  onHistory,
  onRefresh,
}: {
  filters: MemoryFilters;
  list?: MemoryList;
  workspaces: Workspace[];
  selected?: MemoryCard;
  detail?: MemoryDetail;
  sources?: MemorySources;
  history?: MemoryHistory;
  showSources: boolean;
  showHistory: boolean;
  onFilter: (filters: MemoryFilters) => void;
  onSelect: (card: MemoryCard) => void;
  onClose: () => void;
  onSources: (show: boolean) => void;
  onHistory: (show: boolean) => void;
  onRefresh: () => void;
}) {
  const workspace = (id: string) =>
    !id || id === "global"
      ? "全局"
      : workspaces.find((w) => w.id === id)?.name || id;
  const change = (key: keyof MemoryFilters, value: string | number) =>
    onFilter({
      ...filters,
      [key]: value,
      page: key === "page" ? Number(value) : 1,
    });
  const m = detail?.memory;
  return (
    <>
      <Heading title="记忆">把经过核对的经验，带到下一次工作。</Heading>
      <div className="filters memory-filters">
        <input
          type="search"
          id="memory-search"
          aria-label="搜索记忆"
          placeholder="搜索标题、摘要或正文…"
          maxLength={500}
          value={filters.q}
          onChange={(e) => change("q", e.target.value)}
        />
        <select
          id="memory-workspace"
          aria-label="按工作区筛选记忆"
          value={filters.workspace}
          onChange={(e) => change("workspace", e.target.value)}
        >
          <option value="global">全局记忆</option>
          {workspaces.map((w) => (
            <option value={w.id} key={w.id}>
              {w.name} + 全局
            </option>
          ))}
          <option value="*">全部工作区</option>
        </select>
        <select
          id="memory-category"
          aria-label="按分类筛选记忆"
          value={filters.category}
          onChange={(e) => change("category", e.target.value)}
        >
          <option value="">全部分类</option>
          {Object.entries(categories).map(([v, t]) => (
            <option value={v} key={v}>
              {t}
            </option>
          ))}
        </select>
        <select
          id="memory-status"
          aria-label="按状态筛选记忆"
          value={filters.status}
          onChange={(e) => change("status", e.target.value)}
        >
          {Object.entries(statuses).map(([v, t]) => (
            <option value={v} key={v}>
              {t}
            </option>
          ))}
          <option value="all">全部状态</option>
        </select>
        <button className="button quiet" onClick={onRefresh}>
          刷新
        </button>
      </div>
      <div className="memory-shelf">
        {!list ? (
          <Empty>正在读取记忆…</Empty>
        ) : (
          <>
            <p className="memory-count">{list.total} 条记忆 · 最近更新在前</p>
            <div className="memory-grid">
              {list.cards.map((card) => (
                <MemoryCollectibleCard
                  key={card.id}
                  card={card}
                  workspace={workspace(card.workspace_id)}
                  onSelect={() => onSelect(card)}
                />
              ))}
            </div>
            {!list.cards.length && (
              <Empty>没有匹配的记忆，试试调整搜索或筛选。</Empty>
            )}
            <div className="pagination memory-pagination">
              <button
                className="button quiet"
                disabled={list.page <= 1}
                onClick={() => change("page", list.page - 1)}
              >
                上一页
              </button>
              <span>
                第 {list.page} / {Math.max(1, Math.ceil(list.total / 24))} 页
              </span>
              <button
                className="button quiet"
                disabled={list.page * 24 >= list.total}
                onClick={() => change("page", list.page + 1)}
              >
                下一页
              </button>
            </div>
          </>
        )}
      </div>
      {selected && (
        <CardDetailView
          title={m?.title || "正在读取记忆…"}
          subtitle={
            m
              ? `${categories[m.category]} · ${workspace(m.workspace_id)} · v${m.version} · ${statuses[m.status]}`
              : undefined
          }
          onClose={onClose}
          card={
            <MemoryCollectibleCard
              card={m ? { ...m, updated_at: detail!.updated_at } : selected}
              workspace={workspace(selected.workspace_id)}
            />
          }
        >
          {m ? (
            <>
              {m.status !== "active" && (
                <p className="notice">
                  {statuses[m.status]}
                  ：请先核对适用性，本条不能视作当前有效结论。
                </p>
              )}
              <p className="memory-lead">{m.summary}</p>
              <Markdown content={m.content} />
              <details id="memory-conditions">
                <summary>适用条件与标签</summary>
                <p>{m.applicability?.join("；") || "未单独记录适用条件。"}</p>
                <p>标签：{m.tags?.join("、") || "无"}</p>
                <p>实体：{m.entities?.join("、") || "无"}</p>
                {(m.valid_from || m.valid_until) && (
                  <p>
                    有效时间：
                    <Time value={m.valid_from} empty="未限定" /> →{" "}
                    <Time value={m.valid_until} empty="未限定" />
                  </p>
                )}
              </details>
              <details
                id="memory-sources"
                open={showSources}
                onToggle={(e) => onSources(e.currentTarget.open)}
              >
                <summary id="memory-sources-toggle">来源与证据</summary>
                <EvidenceView evidence={m.evidence} />
                {sources ? (
                  sources.sources.map((s, i) => (
                    <div className="memory-evidence" key={i}>
                      <code className="id">{s.id}</code>
                      <p className="muted">
                        {s.availability === "available"
                          ? "来源可用"
                          : "原文已到期、撤回或不可用；保留来源定位。"}
                      </p>
                      {s.locations?.map((uri, j) => (
                        <p className="memory-location" key={j}>
                          {uri}
                        </p>
                      ))}
                      {s.fragments?.map(
                        (f, j) =>
                          f.content && (
                            <div key={j}>
                              <small>{f.locator}</small>
                              <pre>{f.content}</pre>
                            </div>
                          ),
                      )}
                    </div>
                  ))
                ) : (
                  <p className="muted">展开后读取来源原文。</p>
                )}
              </details>
              <details
                id="memory-history"
                open={showHistory}
                onToggle={(e) => onHistory(e.currentTarget.open)}
              >
                <summary id="memory-history-toggle">版本历史</summary>
                {history ? (
                  history.revisions.map((r, i) => (
                    <details key={i} id={`memory-revision-${r.version}`}>
                      <summary>
                        v{r.version} · <Time value={r.operation.created_at} /> ·{" "}
                        {statuses[r.status]}
                      </summary>
                      <p>
                        操作：{r.operation.kind} · {r.operation.actor}
                      </p>
                      <p>{r.operation.reason}</p>
                      <h3>{r.title}</h3>
                      <p>{r.summary}</p>
                      <Markdown content={r.content} />
                      <EvidenceView evidence={r.evidence} />
                    </details>
                  ))
                ) : (
                  <p className="muted">展开后读取版本及操作记录。</p>
                )}
              </details>
              <code className="id">{m.id}</code>
            </>
          ) : (
            <Empty>正在读取完整正文…</Empty>
          )}
        </CardDetailView>
      )}
    </>
  );
}
