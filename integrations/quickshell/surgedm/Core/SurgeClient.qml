import QtQuick
import Quickshell.Io
import "SurgeModel.js" as Model

Item {
  id: root
  visible: false
  width: 0
  height: 0

  property string surgeCommand: "surge"
  property string host: ""
  property string serviceUnit: "surge.service"
  property string downloadDirectory: ""
  property int pollInterval: 2000
  property bool serviceControlsEnabled: true

  property bool available: false
  property bool loading: false
  property bool actionRunning: false
  property var downloads: []
  property var summary: Model.summarize([])
  property string errorText: ""
  property string actionMessage: ""
  property double lastUpdatedAt: 0
  readonly property string label: Model.barLabel(available, loading, summary)

  property bool refreshPending: false
  property bool pollExited: false
  property bool pollStdoutDone: false
  property bool pollStderrDone: false
  property int pollExitCode: -1
  property string pollOutput: ""
  property string pollError: ""

  property bool actionExited: false
  property bool actionStdoutDone: false
  property bool actionStderrDone: false
  property int actionExitCode: -1
  property string actionOutput: ""
  property string actionErrorOutput: ""
  property string actionSuccessMessage: ""

  signal changed()

  function commandWithConnection(args) {
    var command = [root.surgeCommand]
    var target = String(root.host || "").trim()
    if (target !== "") command.push("--host", target)
    return command.concat(args)
  }

  function refresh() {
    if (pollProcess.running) {
      refreshPending = true
      return
    }

    pollExited = false
    pollStdoutDone = false
    pollStderrDone = false
    pollExitCode = -1
    pollOutput = ""
    pollError = ""
    loading = true
    pollProcess.command = commandWithConnection(["ls", "--json", "--server-only"])
    pollProcess.running = true
  }

  function finishPollIfReady() {
    if (!pollExited || !pollStdoutDone || !pollStderrDone) return

    loading = false
    if (pollExitCode === 0) {
      try {
        downloads = Model.parseDownloads(pollOutput)
        summary = Model.summarize(downloads)
        available = true
        errorText = ""
        lastUpdatedAt = Date.now()
        changed()
      } catch (error) {
        available = false
        errorText = "Surge returned data the widget could not read."
      }
    } else {
      available = false
      downloads = []
      summary = Model.summarize([])
      errorText = Model.connectionError(pollExitCode, pollError, pollOutput)
      changed()
    }

    if (refreshPending) {
      refreshPending = false
      Qt.callLater(refresh)
    }
  }

  function runAction(args, successMessage) {
    if (actionProcess.running) return false

    actionExited = false
    actionStdoutDone = false
    actionStderrDone = false
    actionExitCode = -1
    actionOutput = ""
    actionErrorOutput = ""
    actionSuccessMessage = successMessage
    actionMessage = ""
    actionRunning = true
    actionProcess.command = commandWithConnection(args)
    actionProcess.running = true
    return true
  }

  function add(url) {
    var value = String(url || "").trim()
    if (value === "") {
      actionMessage = "Enter a download URL."
      return false
    }
    var args = ["add", value]
    var output = String(downloadDirectory || "").trim()
    if (output !== "") args.push("--output", output)
    return runAction(args, "Download added.")
  }

  function pause(id) {
    return runAction(["pause", String(id)], "Download paused.")
  }

  function resume(id) {
    return runAction(["resume", String(id)], "Download resumed.")
  }

  function serviceAction(verb) {
    if (!serviceControlsEnabled || actionProcess.running) return false
    var allowed = { start: true, stop: true, restart: true }
    if (!allowed[verb]) return false

    actionExited = false
    actionStdoutDone = false
    actionStderrDone = false
    actionExitCode = -1
    actionOutput = ""
    actionErrorOutput = ""
    actionSuccessMessage = "Service " + verb + " requested."
    actionMessage = ""
    actionRunning = true
    actionProcess.command = ["systemctl", "--user", verb, root.serviceUnit]
    actionProcess.running = true
    return true
  }

  function finishActionIfReady() {
    if (!actionExited || !actionStdoutDone || !actionStderrDone) return

    actionRunning = false
    var error = Model.actionError(actionExitCode, actionErrorOutput, actionOutput)
    actionMessage = error === "" ? actionSuccessMessage : error
    settleRefresh.restart()
  }

  Process {
    id: pollProcess
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        root.pollOutput = String(text || "")
        root.pollStdoutDone = true
        root.finishPollIfReady()
      }
    }
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        root.pollError = String(text || "")
        root.pollStderrDone = true
        root.finishPollIfReady()
      }
    }
    onExited: function(exitCode) {
      root.pollExitCode = exitCode
      root.pollExited = true
      root.finishPollIfReady()
    }
  }

  Process {
    id: actionProcess
    stdout: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        root.actionOutput = String(text || "")
        root.actionStdoutDone = true
        root.finishActionIfReady()
      }
    }
    stderr: StdioCollector {
      waitForEnd: true
      onStreamFinished: {
        root.actionErrorOutput = String(text || "")
        root.actionStderrDone = true
        root.finishActionIfReady()
      }
    }
    onExited: function(exitCode) {
      root.actionExitCode = exitCode
      root.actionExited = true
      root.finishActionIfReady()
    }
  }

  Timer {
    interval: Math.max(500, root.pollInterval)
    running: true
    repeat: true
    triggeredOnStart: true
    onTriggered: root.refresh()
  }

  Timer {
    id: settleRefresh
    interval: 500
    repeat: false
    onTriggered: root.refresh()
  }
}
