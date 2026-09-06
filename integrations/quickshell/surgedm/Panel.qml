import QtQuick
import QtQuick.Controls
import QtQuick.Layouts
import Quickshell.Io
import qs.Commons
import qs.Ui
import "Core" as SurgeCore
import "Core/SurgeModel.js" as Model

// Omarchy bar widget for a local Surge server.
//
// The bar face is one glyph; the popup carries server status, an add field,
// and a row per download. Every value shown comes from the Surge CLI through
// Core/SurgeClient.qml (argv arrays, never a shell string, and the token is
// never read here); everything painted comes from the active Omarchy theme
// through Color/Style and the bar's live properties.
//
// This file is the plugin's only entry point (manifest entryPoints.barWidget).
// Per-widget settings arrive inline on the widget's shell.json entry and are
// read through Panel.setting(); their names and defaults are declared in
// manifest.json under barWidget.defaults/barWidget.schema.
Panel {
  id: root
  moduleName: "io.github.surgedm.desktop"
  ipcTarget: "io.github.surgedm.desktop"
  manageIpc: false

  readonly property color foreground: bar ? bar.foreground : Color.foreground
  readonly property color urgent: bar ? bar.urgent : Color.urgent
  readonly property color accent: Color.accent
  readonly property color dim: Qt.darker(foreground, 1.4)
  readonly property string fontFamily: bar ? bar.fontFamily : Style.font.family

  // Row cursor. `cursorActive` stays false until the user actually drives the
  // panel, so opening it with the mouse paints no cursor ring.
  property bool cursorActive: false
  property int rowIndex: 0

  readonly property var downloads: surge.downloads || []
  readonly property bool serviceControls: surge.serviceControlsEnabled

  readonly property string heroMeta: {
    if (!surge.available) return surge.loading ? "Connecting" : "Offline"
    var s = surge.summary
    if (s.active > 0) return s.active + " active · " + Model.formatSpeed(s.speed)
    if (s.paused > 0) return s.paused + " paused · " + s.total + " total"
    return s.total === 1 ? "1 download" : s.total + " downloads"
  }

  readonly property string serviceHint: surge.available
    ? "Stop " + surge.serviceUnit
    : "Start " + surge.serviceUnit

  function statusColor(status) {
    var value = String(status || "").toLowerCase()
    if (value === "failed" || value === "error") return root.urgent
    if (value === "completed" || Model.isActive(value)) return root.accent
    return root.dim
  }

  function statusGlyph(status) {
    var value = String(status || "").toLowerCase()
    if (value === "failed" || value === "error") return "󰅙"
    if (value === "completed") return "󰄬"
    if (Model.isPaused(value)) return "󰏤"
    return "󰇚"
  }

  // These read `surge.downloads` rather than `root.downloads`: the client
  // emits downloadsChanged while this root's own bindings are still being
  // evaluated, so the property can legitimately be undefined at that point.
  function downloadRows() {
    return surge.downloads || []
  }

  function clampCursor() {
    var count = downloadRows().length
    if (count === 0) {
      rowIndex = 0
      return
    }
    if (rowIndex >= count) rowIndex = count - 1
    if (rowIndex < 0) rowIndex = 0
  }

  function moveCursor(dy) {
    var count = downloadRows().length
    if (dy === 0 || count === 0) return
    rowIndex = Math.max(0, Math.min(count - 1, rowIndex + dy))
    scrollRowIntoView()
  }

  function setRowCursor(index) {
    cursorActive = true
    rowIndex = index
  }

  function selectedDownload() {
    var rows = downloadRows()
    if (rowIndex < 0 || rowIndex >= rows.length) return null
    return rows[rowIndex]
  }

  // Enter/Space and a row click do the same thing: whatever the row's own
  // button offers. Completed and failed rows have no start/stop action.
  function toggleDownload(item) {
    if (!item || surge.actionRunning) return
    if (Model.isPaused(item.status)) surge.resume(item.id)
    else if (Model.isActive(item.status)) surge.pause(item.id)
  }

  function deleteSelected() {
    var item = selectedDownload()
    if (item && !surge.actionRunning) surge.removeDownload(item.id)
  }

  function toggleService() {
    if (!surge.serviceControlsEnabled) return
    surge.serviceAction(surge.available ? "stop" : "start")
  }

  // Hand keys back to the key catcher after a submit. Without this the panel
  // stays open with focus on a field nobody is typing in, and Esc/Tab/j/k/x
  // are dead until the user clicks. Stock network/Panel.qml does the same.
  function focusKeyCatcher() {
    if (root.opened) keyCatcher.forceActiveFocus()
  }

  function submitUrl() {
    if (!surge.add(urlField.text)) return
    urlField.text = ""
    Qt.callLater(root.focusKeyCatcher)
  }

  function scrollRowIntoView() {
    if (!rowColumn || rowIndex < 0 || rowIndex >= rowColumn.children.length) return
    var item = rowColumn.children[rowIndex]
    if (!item) return
    Qt.callLater(function() {
      if (!item) return
      var margin = Style.space(6)
      var top = item.mapToItem(panelFlick.contentItem, 0, 0).y
      var bottom = top + item.height
      var viewTop = panelFlick.contentY
      var viewBottom = viewTop + panelFlick.height
      var maxY = Math.max(0, panelFlick.contentHeight - panelFlick.height)
      if (top < viewTop + margin) panelFlick.contentY = Math.max(0, top - margin)
      else if (bottom > viewBottom - margin)
        panelFlick.contentY = Math.min(maxY, bottom + margin - panelFlick.height)
    })
  }

  implicitWidth: button.implicitWidth
  implicitHeight: button.implicitHeight

  onOpenedChanged: if (opened) {
    cursorActive = false
    panelFlick.contentY = 0
    surge.refresh()
  }

  SurgeCore.SurgeClient {
    id: surge
    surgeCommand: String(root.setting("command", "surge"))
    host: String(root.setting("host", ""))
    serviceUnit: String(root.setting("serviceUnit", "surge.service"))
    downloadDirectory: String(root.setting("downloadDirectory", ""))
    // Omarchy declares the interval in seconds (manifest barWidget.schema);
    // the reusable client speaks milliseconds.
    pollInterval: Math.max(1, Number(root.setting("refreshIntervalSec", 2)) || 2) * 1000
    serviceControlsEnabled: root.setting("serviceControlsEnabled", true) === true
    onDownloadsChanged: root.clampCursor()
  }

  IpcHandler {
    target: root.ipcTarget

    function open(): void { root.open() }
    function close(): void { root.close() }
    function show(): void { root.open() }
    function hide(): void { root.close() }
    function toggle(): void { root.toggle() }
    function refresh(): string { surge.refresh(); return "ok" }
    function status(): string { return surge.label }
  }

  BarIconButton {
    id: button
    anchors.fill: parent
    bar: root.bar
    text: "󰇚"
    foreground: root.barForeground
    // Accent while downloads are moving, dimmed while the server is down.
    active: surge.available && surge.summary.active > 0
    activeColor: root.accent
    dimmed: !surge.available
    tooltipText: surge.available
      ? surge.label
      : (surge.errorText !== "" ? surge.errorText : "Surge offline")
    onPressed: function(mouseButton) {
      if (mouseButton === Qt.RightButton) root.toggleService()
      else if (mouseButton === Qt.MiddleButton) surge.refresh()
      else root.toggle()
    }
  }

  KeyboardPanel {
    id: panel
    anchorItem: button
    owner: root
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(340))
    contentHeight: panel.fittedContentHeight(column.implicitHeight, Style.space(520))

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      // The URL field owns every key while it has focus.
      blocked: urlField.activeFocus
      onMoveRequested: function(dx, dy) {
        if (!root.cursorActive) {
          root.cursorActive = true
          return
        }
        root.moveCursor(dy)
      }
      // With no row selected, Enter/Space enters the URL field (the weather
      // panel's Return-to-edit pattern); Esc in the field hands keys back.
      onActivateRequested: {
        if (root.cursorActive) root.toggleDownload(root.selectedDownload())
        else urlField.forceActiveFocus()
      }
      onDeleteRequested: if (root.cursorActive) root.deleteSelected()
      onCloseRequested: root.close()
      onTabRequested: function(direction) { root.switchPanel(direction) }
      onTextKey: function(text) {
        var key = String(text).toLowerCase()
        if (key === "r") surge.refresh()
        else if (key === "s") root.toggleService()
      }

      Flickable {
        id: panelFlick
        anchors.fill: parent
        contentWidth: width
        contentHeight: column.implicitHeight
        clip: true
        boundsBehavior: Flickable.StopAtBounds
        flickableDirection: Flickable.VerticalFlick
        interactive: contentHeight > height
        ScrollBar.vertical: ScrollBar { policy: ScrollBar.AsNeeded }

        Column {
          id: column
          width: panelFlick.width
          spacing: Style.space(12)

          PanelHero {
            id: hero
            width: parent.width
            title: "Surge"
            meta: root.heroMeta
            foreground: root.foreground
            fontFamily: root.fontFamily
            iconOpacity: surge.available ? 1.0 : 0.5
            iconComponent: Component {
              Text {
                textFormat: Text.PlainText
                text: "󰇚"
                color: surge.available ? root.foreground : root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.display
              }
            }

            // The switch owns the service, mouse and keyboard alike; `s`
            // and a right-click on the bar face hit the same function.
            trailingControl: Component {
              ToggleSwitch {
                id: powerSwitch
                visible: root.serviceControls
                checked: surge.available
                // Only an in-flight action, never a background poll: `busy`
                // swallows clicks (Ui/ToggleSwitch.qml), and a slow remote
                // host keeps `loading` true for most of every interval.
                busy: surge.actionRunning
                foreground: hero.foreground
                onToggled: root.toggleService()

                PanelToolTip {
                  visible: powerSwitch.containsMouse
                  text: root.serviceHint
                  fontFamily: hero.fontFamily
                }
              }
            }
          }

          Text {
            textFormat: Text.PlainText
            visible: surge.errorText !== "" || surge.actionMessage !== ""
            width: parent.width
            text: surge.errorText !== "" ? surge.errorText : surge.actionMessage
            color: surge.errorText !== "" ? root.urgent : root.dim
            font.family: root.fontFamily
            font.pixelSize: Style.font.bodySmall
            wrapMode: Text.WordWrap
          }

          RowLayout {
            width: parent.width
            spacing: Style.spacing.controlGap

            // Deliberately not disabled while an action runs: disabling a
            // focused Item clears its activeFocus, which would strand the
            // panel's keyboard handling. `SurgeClient.runAction` refuses a
            // second action on its own.
            TextField {
              id: urlField
              Layout.fillWidth: true
              foreground: root.foreground
              accent: root.accent
              placeholderText: "Paste a download URL"
              onAccepted: root.submitUrl()
              Keys.onEscapePressed: root.focusKeyCatcher()
            }

            PanelActionButton {
              iconText: "󰄬"
              tooltipText: "Add download"
              foreground: root.foreground
              fontFamily: root.fontFamily
              enabled: !surge.actionRunning && urlField.text.trim() !== ""
              Layout.alignment: Qt.AlignVCenter
              onClicked: root.submitUrl()
            }

            PanelActionButton {
              iconText: "󰑐"
              tooltipText: "Refresh now"
              foreground: root.foreground
              fontFamily: root.fontFamily
              enabled: !surge.loading
              Layout.alignment: Qt.AlignVCenter
              onClicked: surge.refresh()
            }
          }

          PanelSeparator {
            foreground: root.foreground
          }

          Column {
            width: parent.width
            spacing: Style.space(10)

            Item {
              width: parent.width
              implicitHeight: sectionHeader.implicitHeight

              PanelSectionHeader {
                id: sectionHeader
                anchors.left: parent.left
                anchors.verticalCenter: parent.verticalCenter
                text: "DOWNLOADS"
                foreground: root.foreground
                fontFamily: root.fontFamily
              }

              Text {
                textFormat: Text.PlainText
                anchors.right: parent.right
                anchors.verticalCenter: parent.verticalCenter
                visible: surge.available
                text: surge.summary.total + " total"
                color: root.dim
                font.family: root.fontFamily
                font.pixelSize: Style.font.caption
              }
            }

            Text {
              textFormat: Text.PlainText
              visible: !surge.available
              width: parent.width
              text: surge.loading
                ? "Connecting to Surge."
                : (root.serviceControls
                  ? "Start the user service to connect."
                  : "Check the Surge host and authentication settings.")
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              wrapMode: Text.WordWrap
              horizontalAlignment: Text.AlignHCenter
            }

            Text {
              textFormat: Text.PlainText
              visible: surge.available && root.downloads.length === 0
              width: parent.width
              text: "No downloads yet. Paste a URL above to start one."
              color: root.dim
              font.family: root.fontFamily
              font.pixelSize: Style.font.body
              wrapMode: Text.WordWrap
              horizontalAlignment: Text.AlignHCenter
            }

            Column {
              id: rowColumn
              visible: surge.available && root.downloads.length > 0
              width: parent.width
              spacing: Style.space(6)

              Repeater {
                model: root.downloads

                DownloadRow {
                  required property var modelData
                  required property int index
                  width: rowColumn.width
                  download: modelData
                  rowIndex: index
                }
              }
            }
          }
        }
      }
    }
  }

  component DownloadRow: CursorSurface {
    id: downloadRow
    property var download: null
    property int rowIndex: 0

    readonly property string status: download ? String(download.status || "") : ""
    readonly property real progress: download ? Math.max(0, Math.min(100, Number(download.progress) || 0)) : 0
    readonly property bool canToggle: Model.isPaused(status) || Model.isActive(status)
    readonly property color stateColor: root.statusColor(status)

    hasCursor: root.cursorActive && root.rowIndex === rowIndex
    foreground: root.foreground

    implicitHeight: rowContent.implicitHeight + Style.spacing.rowPaddingX

    MouseArea {
      anchors.fill: parent
      hoverEnabled: true
      cursorShape: downloadRow.canToggle ? Qt.PointingHandCursor : Qt.ArrowCursor
      onEntered: root.setRowCursor(downloadRow.rowIndex)
      onClicked: root.toggleDownload(downloadRow.download)
    }

    ColumnLayout {
      id: rowContent
      anchors.left: parent.left
      anchors.right: parent.right
      anchors.verticalCenter: parent.verticalCenter
      anchors.leftMargin: Style.space(10)
      anchors.rightMargin: Style.space(10)
      spacing: Style.space(6)

      RowLayout {
        Layout.fillWidth: true
        spacing: Style.space(8)

        Text {
          textFormat: Text.PlainText
          text: root.statusGlyph(downloadRow.status)
          color: downloadRow.stateColor
          font.family: root.fontFamily
          font.pixelSize: Style.font.icon
          Layout.alignment: Qt.AlignVCenter
        }

        ColumnLayout {
          Layout.fillWidth: true
          spacing: Style.space(1)

          Text {
            textFormat: Text.PlainText
            Layout.fillWidth: true
            text: downloadRow.download ? String(downloadRow.download.filename) : ""
            color: root.foreground
            font.family: root.fontFamily
            font.pixelSize: Style.font.body
            elide: Text.ElideMiddle
          }

          Text {
            textFormat: Text.PlainText
            Layout.fillWidth: true
            text: Model.statusLabel(downloadRow.status)
              + " · " + Math.round(downloadRow.progress) + "%"
              + (downloadRow.download && downloadRow.download.speed > 0
                ? " · " + Model.formatSpeed(downloadRow.download.speed)
                : "")
            color: downloadRow.stateColor
            font.family: root.fontFamily
            font.pixelSize: Style.font.caption
            elide: Text.ElideRight
          }
        }

        PanelActionButton {
          visible: downloadRow.canToggle
          iconText: Model.isPaused(downloadRow.status) ? "󰐊" : "󰏤"
          tooltipText: Model.isPaused(downloadRow.status) ? "Resume" : "Pause"
          foreground: root.foreground
          fontFamily: root.fontFamily
          enabled: !surge.actionRunning
          Layout.alignment: Qt.AlignVCenter
          onClicked: root.toggleDownload(downloadRow.download)
        }

        PanelActionButton {
          iconText: "󰅙"
          tooltipText: "Delete"
          foreground: root.foreground
          hoverColor: root.urgent
          fontFamily: root.fontFamily
          enabled: !surge.actionRunning
          Layout.alignment: Qt.AlignVCenter
          onClicked: if (downloadRow.download) surge.removeDownload(downloadRow.download.id)
        }
      }

      Rectangle {
        Layout.fillWidth: true
        implicitHeight: Math.max(1, Style.space(3))
        radius: implicitHeight / 2
        color: Util.alpha(root.foreground, 0.14)

        // No width Behavior: the Repeater model is a fresh array every poll,
        // so delegates are rebuilt rather than animated.
        Rectangle {
          width: parent.width * downloadRow.progress / 100
          height: parent.height
          radius: parent.radius
          color: downloadRow.stateColor
        }
      }
    }
  }
}
