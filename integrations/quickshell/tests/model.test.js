"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const modelPath = path.join(__dirname, "..", "surgedm", "Core", "SurgeModel.js");
const source = fs.readFileSync(modelPath, "utf8").replace(/^\.pragma library\s*/, "");
const model = { JSON, Math, Number, String, Array, Error, isFinite };
vm.createContext(model);
vm.runInContext(source, model, { filename: modelPath });

const downloads = model.parseDownloads(JSON.stringify([
  {
    id: "one",
    filename: "one.iso",
    status: "downloading",
    progress: 150,
    speed: 1048576,
    downloaded: 50,
    total_size: 100
  },
  {
    id: "two",
    filename: "two.iso",
    status: "paused",
    progress: 25,
    speed: -1,
    downloaded: 25,
    total_size: 100
  }
]));

assert.equal(downloads.length, 2);
assert.equal(downloads[0].progress, 100);
assert.equal(downloads[1].speed, 0);

const summary = model.summarize(downloads);
assert.equal(summary.total, 2);
assert.equal(summary.active, 1);
assert.equal(summary.paused, 1);
assert.equal(summary.speed, 1048576);
assert.equal(summary.progress, 62.5);

assert.equal(model.formatBytes(1024), "1.0 KiB");
assert.equal(model.formatSpeed(1048576), "1.0 MiB/s");
assert.equal(model.statusLabel("downloading"), "Downloading");
assert.equal(model.statusLabel("paused"), "Paused");
assert.equal(model.statusLabel("failed"), "Failed");
assert.equal(model.statusLabel("custom-state"), "Custom state");
assert.equal(model.barLabel(true, false, summary), "1 ↓  1.0 MiB/s");
assert.equal(model.barLabel(false, false, summary), "Surge offline");

assert.equal(
  model.connectionError(1, "surge is not running locally", ""),
  "Surge is offline."
);
assert.match(
  model.connectionError(1, "401 Unauthorized", ""),
  /rejected the token/
);
assert.throws(() => model.parseDownloads("{}"), /non-array/);

// Argument shapes. These guard the CLI argv the widget builds: an operand
// beginning with `-` would be parsed as a flag (e.g. `surge add --batch=FILE`
// reads a file, `surge rm --clean` wipes every completed record).
assert.equal(model.isDownloadUrl("https://example.com/a.iso"), true);
assert.equal(model.isDownloadUrl("http://example.com/a.iso"), true);
assert.equal(model.isDownloadUrl("--batch=/home/u/.local/state/surge/token"), false);
assert.equal(model.isDownloadUrl("file:///etc/passwd"), false);
assert.equal(model.isDownloadUrl("example.com/a.iso"), false);
assert.equal(model.isDownloadUrl(""), false);

assert.equal(model.isDownloadId("abc-123_x.4:5"), true);
assert.equal(model.isDownloadId("--clean"), false);
assert.equal(model.isDownloadId(""), false);
assert.equal(model.isDownloadId("a b"), false);

assert.equal(model.isServiceUnit("surge.service"), true);
assert.equal(model.isServiceUnit("surge@user.service"), true);
assert.equal(model.isServiceUnit("-H root@remote"), false);
assert.equal(model.isServiceUnit("surge"), false);

assert.equal(model.isAbsolutePath("/home/u/Downloads"), true);
assert.equal(model.isAbsolutePath("~/Downloads"), false);
assert.equal(model.isAbsolutePath("Downloads"), false);

// A server-supplied id that cannot be used as an operand must not become a row
// with buttons that would run the wrong command.
const hostile = model.parseDownloads(JSON.stringify([
  { id: "--clean", filename: "evil.iso", status: "downloading" },
  { id: "", filename: "nameless.iso", status: "paused" },
  { id: "good-1", filename: "ok.iso", status: "downloading" }
]));
assert.equal(hostile.length, 1);
assert.equal(hostile[0].id, "good-1");

// Diagnostics reach the always-visible bar tooltip: drop URL userinfo and cap
// the length instead of pasting a raw stderr line onto the bar.
assert.equal(
  model.actionError(1, "dial https://user:secret@example.com:1700 failed", ""),
  "dial https://example.com:1700 failed"
);
const long = model.connectionError(1, "x".repeat(400), "");
assert.equal(long.length, 160);
assert.equal(long.slice(-1), "…");

console.log("SurgeModel tests passed");
