import { useEffect, useRef, useState } from "react";
import { get } from "../api";
import type { Meta } from "../types";

export function localVersionStatus(
  meta?: Pick<Meta, "build" | "installed_build">,
) {
  if (
    !meta ||
    !meta.build ||
    !meta.installed_build ||
    meta.build === "unknown" ||
    meta.installed_build === "unknown"
  )
    return { state: "unknown", text: "本机版本未确认" };
  return meta.build === meta.installed_build
    ? { state: "current", text: "本机已同步" }
    : { state: "update", text: "本机程序已更换，等待加载" };
}

export interface TagStatus {
  state:
    | "current"
    | "update"
    | "ahead"
    | "unknown"
    | "unavailable"
    | "not_found";
  latest_version?: string;
  checked_at: string;
  tag_url?: string;
}

export function tagStatusText(result?: TagStatus) {
  if (!result) return "尚未检查 tag";
  switch (result.state) {
    case "current":
      return "版本号与最新 tag 一致";
    case "update":
      return `新版本 ${result.latest_version}`;
    case "ahead":
      return "当前版本高于最新 tag";
    case "not_found":
      return "未找到可比较的 tag";
    case "unavailable":
      return "检查失败，暂无法确认";
    default:
      return "当前版本无法与 tag 比较";
  }
}

export function VersionPanel({
  meta,
  connected,
}: {
  meta?: Meta;
  connected: boolean;
}) {
  const [result, setResult] = useState<TagStatus>();
  const [checking, setChecking] = useState(false);
  const request = useRef(0);
  const version = meta?.version;
  const build = meta?.build;
  const local = localVersionStatus(meta);

  async function check() {
    const id = ++request.current;
    setChecking(true);
    try {
      const response = await get<TagStatus>("version/check");
      if (id === request.current) setResult(response.data);
    } catch {
      if (id === request.current)
        setResult({
          state: "unavailable",
          checked_at: new Date().toISOString(),
        });
    } finally {
      if (id === request.current) setChecking(false);
    }
  }

  useEffect(() => {
    setResult(undefined);
    setChecking(false);
    if (connected && version) void check();
    return () => {
      request.current++;
    };
  }, [connected, version, build]);

  return (
    <div className="sidebar-version" aria-label="版本与更新">
      <div className="version-heading">
        <span>当前版本</span>
        <strong>{version ? `v${version.replace(/^v/, "")}` : "读取中…"}</strong>
      </div>
      {build && (
        <code className="version-build" title={`运行构建：${build}`}>
          {build}
        </code>
      )}
      <p className={`version-local ${local.state}`}>
        <span className="version-indicator" />
        {connected ? local.text : "连接恢复后确认版本"}
      </p>
      <div className="version-tag" role="status" aria-live="polite">
        {checking ? "正在检查 tag…" : tagStatusText(result)}
      </div>
      <div className="version-actions">
        <button
          className="version-check"
          disabled={!connected || checking}
          onClick={() => void check()}
        >
          {checking ? "检查中…" : "检查更新"}
        </button>
        {result?.tag_url && (
          <a href={result.tag_url} target="_blank" rel="noreferrer">
            查看 tag
          </a>
        )}
      </div>
      <small
        className="version-source"
        title={
          result?.checked_at
            ? `检查时间：${new Date(result.checked_at).toLocaleString()}`
            : undefined
        }
      >
        对照 GitHub 最新 tag（含预发布）
      </small>
    </div>
  );
}

// Version availability is independent of failures in a page's business query.
export function VersionStatus() {
  const [meta, setMeta] = useState<Meta>();
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    let controller: AbortController | undefined;
    async function refresh() {
      clearTimeout(timer);
      controller?.abort();
      if (cancelled) return;
      if (document.hidden) {
        setMeta(undefined);
        return;
      }
      const current = new AbortController();
      controller = current;
      try {
        const response = await get<Meta>("meta", {
          signal: AbortSignal.any([current.signal, AbortSignal.timeout(10000)]),
        });
        if (!cancelled && !current.signal.aborted && !document.hidden)
          setMeta(response.data);
      } catch {
        // An unavailable service must not retain a stale freshness claim.
        if (!cancelled && !current.signal.aborted) setMeta(undefined);
      } finally {
        if (!cancelled && !current.signal.aborted && !document.hidden)
          timer = setTimeout(refresh, 15000);
      }
    }
    void refresh();
    document.addEventListener("visibilitychange", refresh);
    return () => {
      cancelled = true;
      controller?.abort();
      clearTimeout(timer);
      document.removeEventListener("visibilitychange", refresh);
    };
  }, []);
  return <VersionPanel meta={meta} connected={!!meta} />;
}
