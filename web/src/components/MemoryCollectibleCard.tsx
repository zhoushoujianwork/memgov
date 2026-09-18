// Adapted from relayer-next's MemoryCollectibleCard. See ../../LICENSE-relayer.
import type { MemoryCard } from "../types";
import { Time } from "./common";

export const categories = {
  fact: "事实",
  preference: "偏好",
  constraint: "约束",
  decision: "决策",
  procedure: "流程",
  lesson: "经验",
};
export const statuses: Record<string, string> = {
  active: "有效",
  disputed: "有争议",
  retired: "已退役",
  superseded: "已被替代",
};
const symbols = {
  fact: "◈",
  preference: "♡",
  constraint: "◇",
  decision: "◆",
  procedure: "≡",
  lesson: "✧",
};
export function MemoryCollectibleCard({
  card,
  workspace,
  onSelect,
}: {
  card: MemoryCard;
  workspace: string;
  onSelect?: () => void;
}) {
  const chars = Array.from(card.summary || ""),
    summary = chars.slice(0, 100).join("") + (chars.length > 100 ? "…" : "");
  const content = (
    <>
      <span className="collectible-art" aria-hidden="true">
        <span className="collectible-sigil">{symbols[card.category]}</span>
      </span>
      <span className="collectible-type">
        {categories[card.category]}
        <span>{workspace}</span>
      </span>
      <strong className="collectible-title" title={card.title}>
        {card.title}
      </strong>
      <span className="collectible-summary">{summary}</span>
      <span className="collectible-status">
        {statuses[card.status] || card.status}
        <span>v{card.version}</span>
      </span>
      <small className="collectible-updated">
        <Time value={card.updated_at} prefix="更新于 " />
      </small>
    </>
  );
  const className = `memory-collectible memory-${card.category} memory-state-${card.status}`;
  return onSelect ? (
    <button
      type="button"
      id={`memory-card-${card.id}`}
      className={className}
      data-card-family="memory"
      data-interactive="true"
      aria-label={`查看${categories[card.category]}记忆：${card.title}`}
      onClick={onSelect}
    >
      {content}
    </button>
  ) : (
    <article className={className} data-card-family="memory">
      {content}
    </article>
  );
}
