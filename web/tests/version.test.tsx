import assert from "node:assert/strict";
import test from "node:test";
import { renderToStaticMarkup } from "react-dom/server";
import {
  localVersionStatus,
  tagStatusText,
  VersionPanel,
} from "../src/components/VersionStatus";
import type { Meta } from "../src/types";

test("local sync never implies a latest published version", () => {
  assert.equal(
    localVersionStatus({ build: "abc", installed_build: "abc" }).text,
    "本机已同步",
  );
  assert.equal(
    localVersionStatus({ build: "abc", installed_build: "def" }).state,
    "update",
  );
  assert.equal(
    localVersionStatus({ build: "unknown", installed_build: "unknown" }).state,
    "unknown",
  );
  assert.equal(localVersionStatus(undefined).state, "unknown");
  for (const state of ["not_found", "unavailable", "unknown"] as const)
    assert.ok(!tagStatusText({ state, checked_at: "" }).includes("已是最新"));
});

test("sidebar displays version and build; offline controls cannot assert freshness", () => {
  const meta = {
    version: "2.0.0-rc1",
    build: "abcdef123456",
    installed_build: "abcdef123456",
  } as Meta;
  const online = renderToStaticMarkup(<VersionPanel meta={meta} connected />);
  assert.ok(online.includes("v2.0.0-rc1") && online.includes("abcdef123456"));
  assert.ok(online.includes("检查更新") && online.includes("尚未检查 tag"));
  const offline = renderToStaticMarkup(<VersionPanel connected={false} />);
  assert.ok(
    offline.includes("disabled") && offline.includes("连接恢复后确认版本"),
  );
  assert.ok(!offline.includes("本机已同步"));
});
