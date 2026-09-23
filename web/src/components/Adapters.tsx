// Isolated DOM adapters preserve the existing editor and SSE controller behavior.
import { useEffect, useRef } from "react";
import { markdown } from "../shared/markdown.js";
import { mountTerminal, clearTerminal } from "../adapters/terminal.js";
import {
  renderAgentDeclarations,
  dismissAgentEditor,
} from "../adapters/agent-editor.js";
import type { AgentConfig, TaskDetail } from "../types";
export function Markdown({ content }: { content: string }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current!.replaceChildren(markdown(content));
  }, [content]);
  return <div className="markdown-reader" ref={ref} />;
}
export function Terminal({ detail }: { detail: TaskDetail }) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current!.replaceChildren();
    mountTerminal(ref.current, detail);
  }, [detail]);
  useEffect(() => () => clearTerminal(), []);
  return <div ref={ref} />;
}
export function AgentDeclarations({
  config,
  onSaved,
}: {
  config: AgentConfig;
  onSaved: (message: string) => void;
}) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    ref.current!.replaceChildren();
    renderAgentDeclarations(ref.current, config, onSaved);
  }, [config, onSaved]);
  useEffect(() => () => dismissAgentEditor(), []);
  return <div ref={ref} />;
}
