// Small, DOM-only Markdown reader. Unsupported syntax remains readable text.
// No HTML parsing, remote images, app routes or executable links.
export function safeLink(value) {
  return /^(https?:\/\/|mailto:)/i.test(value) &&
    !/[\u0000-\u0020\u007f]/.test(value)
    ? value
    : "";
}
function el(tag, text = "") {
  const e = document.createElement(tag);
  e.textContent = text;
  return e;
}
function inline(parent, text) {
  const tokens =
    /(!?\[([^\]\n]*)\]\(([^\s)]*)\)|`([^`\n]+)`|\*\*([^*\n]+)\*\*|\*([^*\n]+)\*)/g;
  let start = 0;
  for (const m of text.matchAll(tokens)) {
    parent.append(document.createTextNode(text.slice(start, m.index)));
    if (m[2] !== undefined) {
      const url = safeLink(m[3]);
      if (m[0].startsWith("!"))
        parent.append(el("span", `图片：${m[2] || "未提供描述"} `));
      if (url) {
        const a = el("a", m[0].startsWith("!") ? "打开图片链接" : m[2]);
        a.href = url;
        a.target = "_blank";
        a.rel = "noopener noreferrer";
        parent.append(a);
      } else if (!m[0].startsWith("!")) parent.append(el("span", m[2]));
    } else
      parent.append(
        el(m[4] ? "code" : m[5] ? "strong" : "em", m[4] || m[5] || m[6]),
      );
    start = m.index + m[0].length;
  }
  parent.append(document.createTextNode(text.slice(start)));
}
export function markdown(text) {
  const root = el("div");
  root.className = "memory-markdown";
  const lines = String(text || "")
    .replace(/\r\n?/g, "\n")
    .split("\n");
  const blockStart = (line) =>
    /^(#{1,6}\s|```|~~~|\s*[-*+]\s|\s*\d+\.\s|>\s?)/.test(line);
  for (let i = 0; i < lines.length; ) {
    const line = lines[i];
    if (!line.trim()) {
      i++;
      continue;
    }
    const fence = line.match(/^(`{3,}|~{3,})/);
    if (fence) {
      const code = [];
      i++;
      while (i < lines.length && !lines[i].startsWith(fence[1]))
        code.push(lines[i++]);
      if (i < lines.length) i++;
      const pre = el("pre");
      pre.tabIndex = 0;
      pre.append(el("code", code.join("\n")));
      root.append(pre);
      continue;
    }
    const heading = line.match(/^(#{1,6})\s+(.*)$/);
    if (heading) {
      const h = el(`h${Math.min(heading[1].length + 2, 6)}`);
      inline(h, heading[2]);
      root.append(h);
      i++;
      continue;
    }
    if (/^>/.test(line)) {
      const quote = el("blockquote");
      const chunk = [];
      while (i < lines.length && /^>/.test(lines[i]))
        chunk.push(lines[i++].replace(/^>\s?/, ""));
      inline(quote, chunk.join("\n"));
      root.append(quote);
      continue;
    }
    const list = line.match(/^\s*(?:([-*+])|(\d+)\.)\s+(.*)$/);
    if (list) {
      const ordered = Boolean(list[2]);
      const ul = el(ordered ? "ol" : "ul");
      if (ordered) ul.start = Number(list[2]);
      while (i < lines.length) {
        const m = lines[i].match(/^\s*(?:([-*+])|(\d+)\.)\s+(.*)$/);
        if (!m || Boolean(m[2]) !== ordered) break;
        const li = el("li");
        inline(li, m[3]);
        ul.append(li);
        i++;
      }
      root.append(ul);
      continue;
    }
    if (/^\s*([-*_])(?:\s*\1){2,}\s*$/.test(line)) {
      root.append(el("hr"));
      i++;
      continue;
    }
    const paragraph = [line];
    i++;
    while (i < lines.length && lines[i].trim() && !blockStart(lines[i]))
      paragraph.push(lines[i++]);
    const p = el("p");
    inline(p, paragraph.join("\n"));
    root.append(p);
  }
  return root;
}
