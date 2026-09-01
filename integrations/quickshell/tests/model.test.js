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

console.log("SurgeModel tests passed");
