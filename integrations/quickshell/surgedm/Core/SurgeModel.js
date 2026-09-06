.pragma library

function safeString(value) {
  return String(value === undefined || value === null ? "" : value)
}

// Argument shapes the widget is willing to hand to the Surge CLI and to
// systemctl. Both take these as positional operands, so a value starting with
// `-` would be parsed as an option; callers also pass `--` before operands.
var URL_PATTERN = /^https?:\/\/[^\s]+$/i
var ID_PATTERN = /^[A-Za-z0-9_][A-Za-z0-9._:-]{0,127}$/
var UNIT_PATTERN = /^[A-Za-z0-9_][A-Za-z0-9:_.@\\-]{0,219}\.(service|socket|target|timer)$/

function isDownloadUrl(value) {
  return URL_PATTERN.test(safeString(value).trim())
}

// Ids come from the server's JSON, so they are untrusted input: an id of
// `--clean` would turn a per-row delete into `surge rm --clean`.
function isDownloadId(value) {
  return ID_PATTERN.test(safeString(value))
}

function isServiceUnit(value) {
  return UNIT_PATTERN.test(safeString(value).trim())
}

function isAbsolutePath(value) {
  return safeString(value).trim().charAt(0) === "/"
}

function parseDownloads(raw) {
  var text = safeString(raw).trim()
  if (text === "") return []

  var parsed = JSON.parse(text)
  if (!Array.isArray(parsed)) throw new Error("Surge returned a non-array download list")

  return parsed.filter(function(item) {
    // A row whose id is not a usable operand cannot be paused, resumed or
    // removed, so showing it would only offer broken buttons.
    return item && isDownloadId(item.id)
  }).map(function(item) {
    var progress = Number(item.progress)
    var speed = Number(item.speed)
    return {
      id: safeString(item.id),
      filename: safeString(item.filename) || "Unnamed download",
      status: safeString(item.status).toLowerCase() || "unknown",
      progress: isFinite(progress) ? Math.max(0, Math.min(100, progress)) : 0,
      speed: isFinite(speed) && speed > 0 ? speed : 0,
      downloaded: Number(item.downloaded) || 0,
      totalSize: Number(item.total_size) || 0
    }
  })
}

function isActive(status) {
  return status === "downloading" || status === "queued" || status === "starting"
}

function isPaused(status) {
  return status === "paused"
}

function summarize(downloads) {
  var rows = downloads || []
  var active = 0
  var paused = 0
  var speed = 0
  var progressTotal = 0
  var progressCount = 0

  for (var i = 0; i < rows.length; i++) {
    if (isActive(rows[i].status)) active++
    if (isPaused(rows[i].status)) paused++
    speed += Number(rows[i].speed) || 0
    if (rows[i].totalSize > 0) {
      progressTotal += Number(rows[i].progress) || 0
      progressCount++
    }
  }

  return {
    total: rows.length,
    active: active,
    paused: paused,
    speed: speed,
    progress: progressCount > 0 ? progressTotal / progressCount : 0
  }
}

function formatBytes(value) {
  var bytes = Number(value) || 0
  if (bytes < 1024) return Math.round(bytes) + " B"
  var units = ["KiB", "MiB", "GiB", "TiB"]
  var amount = bytes
  var unit = "B"
  for (var i = 0; i < units.length && amount >= 1024; i++) {
    amount /= 1024
    unit = units[i]
  }
  return (amount >= 10 ? amount.toFixed(0) : amount.toFixed(1)) + " " + unit
}

function formatSpeed(value) {
  return formatBytes(value) + "/s"
}

function statusLabel(status) {
  var value = safeString(status).trim().toLowerCase()
  var labels = {
    downloading: "Downloading",
    queued: "Queued",
    starting: "Starting",
    pausing: "Stopping",
    paused: "Paused",
    completed: "Completed",
    failed: "Failed",
    error: "Failed"
  }
  if (labels[value]) return labels[value]
  if (value === "") return "Unknown"
  value = value.replace(/[-_]+/g, " ")
  return value.charAt(0).toUpperCase() + value.slice(1)
}

function barLabel(available, loading, summary) {
  if (loading && !available) return "Surge …"
  if (!available) return "Surge offline"
  if (summary.active > 0) return summary.active + " ↓  " + formatSpeed(summary.speed)
  if (summary.paused > 0) return summary.paused + " paused"
  return "Surge ready"
}

// CLI diagnostics end up in the panel and in the always-visible bar tooltip,
// so strip URL userinfo (a `host` setting may carry credentials) and cap the
// length instead of pasting an arbitrary stderr line onto the bar.
function firstMessageLine(combined) {
  var line = safeString(combined).split("\n")[0].trim()
  line = line.replace(/(\/\/)[^\/\s@]*@/g, "$1")
  if (line.length > 160) line = line.slice(0, 159) + "…"
  return line
}

function connectionError(exitCode, stderrText, stdoutText) {
  var combined = (safeString(stderrText) + "\n" + safeString(stdoutText)).trim()
  var lower = combined.toLowerCase()

  if (lower.indexOf("not found") !== -1 && lower.indexOf("surge") !== -1)
    return "The Surge executable was not found."
  if (lower.indexOf("unauthorized") !== -1 || lower.indexOf("401") !== -1)
    return "Surge rejected the token. Check SURGE_TOKEN or restart the user service."
  if (lower.indexOf("not running locally") !== -1 || lower.indexOf("connection refused") !== -1
      || lower.indexOf("failed to connect") !== -1)
    return "Surge is offline."
  if (exitCode === 0) return ""

  return firstMessageLine(combined) || "Could not connect to Surge."
}

function actionError(exitCode, stderrText, stdoutText) {
  if (exitCode === 0) return ""
  var combined = (safeString(stderrText) + "\n" + safeString(stdoutText)).trim()
  return firstMessageLine(combined) || "The Surge action failed."
}
