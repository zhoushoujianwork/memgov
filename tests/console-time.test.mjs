import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
const source = await readFile(new URL('../web/src/shared/time.js', import.meta.url));
const { relativeTime, updateTimeElement, refreshRelativeTimes } = await import(`data:text/javascript;base64,${source.toString('base64')}`);
const now = Date.parse('2026-09-16T10:00:00Z');
const stamp = seconds => new Date(now + seconds * 1000).toISOString();

test('relative dates handle boundaries, future times, and unavailable records', () => {
  for (const [offset, expected] of [[0, '刚刚'], [-59, '刚刚'], [-60, '1分钟前'], [-3599, '59分钟前'], [-3600, '1小时前'], [-86400, '1天前'], [-2592000, '1个月前'], [-31536000, '1年前'], [30, '即将'], [120, '2分钟后']]) assert.equal(relativeTime(stamp(offset), now), expected);
  assert.equal(relativeTime(null, now), '未记录');
  assert.equal(relativeTime('not-a-date', now), '时间未知');
});

class Time {
  constructor() { this.dataset = {}; this.attributes = new Map(); }
  getAttribute(name) { return this.attributes.get(name); }
  setAttribute(name, value) { this.attributes.set(name, value); }
  removeAttribute(name) { this.attributes.delete(name); }
}
test('exact dates remain accessible and a clock tick updates existing nodes', () => {
  const originalNow = Date.now;
  const element = new Time();
  globalThis.document = { querySelectorAll: () => [element] };
  try {
    Date.now = () => now;
    updateTimeElement(element, stamp(-60), { prefix: '更新于 ' });
    assert.equal(element.textContent, '更新于 1分钟前');
    assert.equal(element.dateTime, stamp(-60));
    assert.ok(element.title.includes('2026'));
    assert.ok(element.attributes.get('aria-label').includes(element.title));
    Date.now = () => now + 3600000;
    refreshRelativeTimes();
    assert.equal(element.textContent, '更新于 1小时前');
    assert.equal(element.dataset.timestamp, stamp(-60));
    updateTimeElement(element, null, { empty: '尚未记录' });
    assert.equal(element.textContent, '尚未记录');
    assert.equal(element.dataset.timestamp, undefined);
    assert.equal(element.attributes.has('aria-label'), false);
    updateTimeElement(element, 'invalid');
    assert.equal(element.textContent, '时间未知');
  } finally { Date.now = originalNow; delete globalThis.document; }
});
