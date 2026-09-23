import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
const source = await readFile(new URL('../web/src/shared/markdown.js', import.meta.url));
const { markdown, safeLink } = await import(`data:text/javascript;base64,${source.toString('base64')}`);

// A minimal DOM sink makes accidental HTML assignment fail immediately. Actual
// browser layout, focus and dialog behavior are checked in the browser smoke.
class Element {
  constructor(tag, text = '') { this.tag = tag; this.textContent = text; this.children = []; }
  append(...children) { this.children.push(...children); }
  set innerHTML(_) { throw new Error('Untrusted Markdown must never use HTML parsing'); }
}
globalThis.document = { createElement: tag => new Element(tag), createTextNode: text => new Element('#text', text) };
function all(root) { return [root, ...root.children.flatMap(all)]; }
function text(root) { return root.textContent + root.children.map(text).join(''); }

test('Markdown keeps dangerous text inert and never loads image resources', () => {
  const source = '<script>alert(1)</script>\n\n[bad](javascript:alert) [data](data:text/html,bad) [file](file:///tmp/private)\n\n![pixel](https://example.org/pixel)\n\n[good](https://example.org/docs)';
  const root = markdown(source), nodes = all(root);
  assert.ok(text(root).includes('<script>alert(1)</script>'));
  assert.equal(nodes.filter(n => ['script', 'img', 'iframe', 'object'].includes(n.tag)).length, 0);
  const links = nodes.filter(n => n.tag === 'a');
  assert.deepEqual(links.map(n => n.href), ['https://example.org/pixel', 'https://example.org/docs']);
  assert.ok(links.every(n => n.rel === 'noopener noreferrer' && n.target === '_blank'));
});

test('common Markdown retains full Unicode content and code as text', () => {
  const prose = '经验🧠发布检查'.repeat(120);
  const root = markdown(`# 经验\n\n${prose}\n\n**重要**与*提示*和\`code\`\n\n- 核对来源\n- 查看结果\n\n3. 第三步\n\n> 原始证据\n\n~~~html\n<img src=x onerror=alert(1)>\n~~~`);
  const tags = all(root).map(n => n.tag);
  for (const tag of ['h3', 'strong', 'em', 'code', 'ul', 'ol', 'li', 'blockquote', 'pre']) assert.ok(tags.includes(tag), tag);
  assert.ok(text(root).includes(prose));
  assert.ok(text(root).includes('<img src=x onerror=alert(1)>'));
  assert.equal(tags.includes('img'), false);
});

test('unsafe link schemes and disguised controls fail closed', () => {
  for (const url of ['javascript:alert(1)', 'data:text/html,x', '/api/private', '//example.org', 'https://example.org/\nunsafe', 'https://example.org/\u0000x', 'file:///tmp/a']) assert.equal(safeLink(url), '', url);
  assert.equal(safeLink('https://example.org/docs?q=中文'), 'https://example.org/docs?q=中文');
  assert.equal(safeLink('mailto:owner@example.org'), 'mailto:owner@example.org');
});
