"use strict";

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const pluginDir = path.join(__dirname, "..", "surgedm");
const manifestPath = path.join(pluginDir, "manifest.json");
const manifest = JSON.parse(fs.readFileSync(manifestPath, "utf8"));

const FIELD_TYPES = ["string", "integer", "number", "boolean", "enum", "multiselect"];
const SECTIONS = ["left", "center", "right"];

function isPlainObject(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function assertNonEmptyString(value, label) {
  assert.equal(typeof value, "string", `${label} must be a string, got ${typeof value}`);
  assert.notEqual(value.trim(), "", `${label} must not be empty`);
}

assert.equal(manifest.schemaVersion, 1, "schemaVersion must be 1");
for (const field of ["id", "name", "version", "description", "author", "license"]) {
  assertNonEmptyString(manifest[field], field);
}
assert.equal(manifest.id, "io.github.surgedm.desktop", "plugin id is part of the install contract");

assert.ok(
  !Object.prototype.hasOwnProperty.call(manifest, "settings"),
  "top-level `settings` is ignored by the Omarchy shell; settings belong in barWidget.defaults/schema"
);

assert.ok(Array.isArray(manifest.kinds), "kinds must be an array");
assert.deepEqual(manifest.kinds, ["bar-widget"], "kinds must contain exactly bar-widget");

assert.ok(isPlainObject(manifest.entryPoints), "entryPoints must be an object");
const barWidgetEntry = manifest.entryPoints.barWidget;
assertNonEmptyString(barWidgetEntry, "entryPoints.barWidget");
assert.ok(
  !path.isAbsolute(barWidgetEntry) && !barWidgetEntry.startsWith("/"),
  `entryPoints.barWidget must be relative, got ${barWidgetEntry}`
);
assert.ok(
  !barWidgetEntry.split(/[\\/]/).includes(".."),
  `entryPoints.barWidget must not escape the plugin directory, got ${barWidgetEntry}`
);
const entryPath = path.resolve(pluginDir, barWidgetEntry);
assert.ok(
  entryPath.startsWith(pluginDir + path.sep),
  `entryPoints.barWidget must resolve inside the plugin directory, got ${entryPath}`
);
assert.ok(fs.existsSync(entryPath), `entryPoints.barWidget target is missing: ${entryPath}`);

const barWidget = manifest.barWidget;
assert.ok(isPlainObject(barWidget), "barWidget must be an object");
for (const field of ["displayName", "description", "category"]) {
  assertNonEmptyString(barWidget[field], `barWidget.${field}`);
}
assert.equal(typeof barWidget.allowMultiple, "boolean", "barWidget.allowMultiple must be a boolean");
assert.ok(
  SECTIONS.includes(barWidget.defaultSection),
  `barWidget.defaultSection must be one of ${SECTIONS.join(", ")}, got ${barWidget.defaultSection}`
);

const defaults = barWidget.defaults;
const schema = barWidget.schema;
assert.ok(isPlainObject(defaults), "barWidget.defaults must be a plain object");
assert.ok(Array.isArray(schema), "barWidget.schema must be an array");

const seen = new Set();
const duplicates = [];
for (const [index, field] of schema.entries()) {
  assert.ok(isPlainObject(field), `barWidget.schema[${index}] must be an object`);
  assertNonEmptyString(field.key, `barWidget.schema[${index}].key`);
  assert.ok(
    FIELD_TYPES.includes(field.type),
    `barWidget.schema[${index}] (${field.key}) type must be one of ${FIELD_TYPES.join(", ")}, got ${field.type}`
  );
  assertNonEmptyString(field.label, `barWidget.schema[${index}] (${field.key}) label`);
  if (seen.has(field.key)) {
    duplicates.push(field.key);
  }
  seen.add(field.key);
}
assert.deepEqual(duplicates, [], `duplicate barWidget.schema keys: ${duplicates.join(", ")}`);

const defaultKeys = Object.keys(defaults);
const undocumented = defaultKeys.filter((key) => !seen.has(key));
const undefaulted = [...seen].filter((key) => !Object.prototype.hasOwnProperty.call(defaults, key));
assert.deepEqual(
  undocumented,
  [],
  `barWidget.defaults keys missing from barWidget.schema: ${undocumented.join(", ")}`
);
assert.deepEqual(
  undefaulted,
  [],
  `barWidget.schema keys missing from barWidget.defaults: ${undefaulted.join(", ")}`
);

for (const field of schema) {
  const value = defaults[field.key];
  const where = `barWidget.defaults.${field.key}`;
  if (field.type === "string" || field.type === "enum") {
    assert.equal(typeof value, "string", `${where} must be a string for type ${field.type}`);
  } else if (field.type === "boolean") {
    assert.equal(typeof value, "boolean", `${where} must be a boolean`);
  } else if (field.type === "multiselect") {
    assert.ok(Array.isArray(value), `${where} must be an array for type multiselect`);
  } else {
    assert.equal(typeof value, "number", `${where} must be a number for type ${field.type}`);
    assert.ok(Number.isFinite(value), `${where} must be finite`);
    if (field.type === "integer") {
      assert.ok(Number.isInteger(value), `${where} must be an integer, got ${value}`);
    }
    const hasMin = typeof field.min === "number";
    const hasMax = typeof field.max === "number";
    if (hasMin && hasMax) {
      assert.ok(field.min <= field.max, `${field.key}: min ${field.min} exceeds max ${field.max}`);
    }
    if (hasMin) {
      assert.ok(value >= field.min, `${where} (${value}) is below min ${field.min}`);
    }
    if (hasMax) {
      assert.ok(value <= field.max, `${where} (${value}) is above max ${field.max}`);
    }
  }
}

assert.ok(
  !fs.existsSync(path.join(pluginDir, "BarWidget.qml")),
  "BarWidget.qml must not exist; the bar face lives in the single Panel.qml entry point"
);

console.log("Omarchy manifest contract tests passed");
