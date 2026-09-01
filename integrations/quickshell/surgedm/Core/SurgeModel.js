.pragma library

function safeString(value) {
  return String(value === undefined || value === null ? "" : value)
}

function parseDownloads(raw) {
  var text = safeString(raw).trim()
  if (text === "") return []

  var parsed = JSON.parse(text)
  if (!Array.isArray(parsed)) throw new Error("Surge returned a non-array download list")

  return parsed.map(function(item) {
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

function barLabel(available, loading, summary) {
  if (loading && !available) return "Surge …"
  if (!available) return "Surge offline"
  if (summary.active > 0) return summary.active + " ↓  " + formatSpeed(summary.speed)
  if (summary.paused > 0) return summary.paused + " paused"
  return "Surge ready"
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

  var firstLine = combined.split("\n")[0]
  return firstLine || "Could not connect to Surge."
}

function actionError(exitCode, stderrText, stdoutText) {
  if (exitCode === 0) return ""
  var combined = (safeString(stderrText) + "\n" + safeString(stdoutText)).trim()
  var firstLine = combined.split("\n")[0]
  return firstLine || "The Surge action failed."
}
