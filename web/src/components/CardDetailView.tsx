// Adapted from relayer-next's reading layout. See ../../LICENSE-relayer.
import { useEffect, useId, useRef, type ReactNode } from "react";
import { createPortal } from "react-dom";

export function CardDetailView({
  title,
  subtitle,
  card,
  children,
  onClose,
}: {
  title: string;
  subtitle?: string;
  card: ReactNode;
  children: ReactNode;
  onClose: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null),
    heading = useId();
  useEffect(() => {
    const trigger =
      document.activeElement instanceof HTMLElement
        ? document.activeElement
        : null;
    const element = dialog.current!,
      previous = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    element.showModal();
    return () => {
      element.close();
      document.body.style.overflow = previous;
      if (trigger?.isConnected) trigger.focus({ preventScroll: true });
    };
  }, []);
  return createPortal(
    <dialog
      ref={dialog}
      className="card-detail-view"
      data-detail-layout="reading"
      aria-labelledby={heading}
      onCancel={(event) => {
        event.preventDefault();
        onClose();
      }}
      onClick={(event) => {
        if (event.target === event.currentTarget) onClose();
      }}
    >
      <div className="card-detail-stage">
        <button
          className="card-detail-close"
          autoFocus
          aria-label="关闭记忆详情"
          onClick={onClose}
        >
          <span>返回</span>
          <kbd>ESC</kbd>
          <span aria-hidden="true">×</span>
        </button>
        <aside className="card-detail-showcase">
          <span className="card-detail-edition">MEMGOV · 记忆</span>
          <div className="card-detail-pedestal">
            <div className="card-detail-face" inert aria-hidden="true">
              {card}
            </div>
          </div>
          <span className="card-detail-boundary">正式记忆 · 只读查阅</span>
        </aside>
        <article
          className="card-detail-content"
          tabIndex={0}
          aria-label="记忆详细内容"
        >
          <header className="card-detail-heading">
            <small>记忆详情</small>
            <h2 id={heading}>{title}</h2>
            {subtitle && <p>{subtitle}</p>}
          </header>
          <div className="card-detail-body">{children}</div>
        </article>
      </div>
    </dialog>,
    document.body,
  );
}
