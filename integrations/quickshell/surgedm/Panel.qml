import QtQuick
import Quickshell.Io
import qs.Commons
import qs.Ui
import "Core" as SurgeCore

Panel {
  id: root
  moduleName: "io.github.surgedm.desktop"
  ipcTarget: "io.github.surgedm.desktop"
  manageIpc: false

  property var anchorItem: null
  property var hostWidget: null
  readonly property var barIdentity: hostWidget || root
  readonly property alias client: surgeClient

  function open() {
    root.controller.show()
    surgeClient.refresh()
  }

  function close() {
    root.controller.hide()
  }

  function toggle() {
    if (root.opened) root.close()
    else root.open()
  }

  function switchPanel(direction) {
    if (root.bar && typeof root.bar.switchPanelFrom === "function")
      return root.bar.switchPanelFrom(root.barIdentity, direction)
    return false
  }

  SurgeCore.SurgeClient {
    id: surgeClient
    surgeCommand: String(root.setting("command", "surge"))
    host: String(root.setting("host", ""))
    serviceUnit: String(root.setting("serviceUnit", "surge.service"))
    downloadDirectory: String(root.setting("downloadDirectory", ""))
    pollInterval: Math.max(500, Number(root.setting("pollInterval", 2000)) || 2000)
    serviceControlsEnabled: root.setting("serviceControlsEnabled", true) === true
  }

  IpcHandler {
    target: root.ipcTarget

    function open(): void { root.open() }
    function close(): void { root.close() }
    function show(): void { root.open() }
    function hide(): void { root.close() }
    function toggle(): void { root.toggle() }
    function refresh(): void { surgeClient.refresh() }
  }

  KeyboardPanel {
    id: panel
    anchorItem: root.anchorItem
    owner: root.barIdentity
    bar: root.bar
    open: root.opened
    focusTarget: keyCatcher
    contentWidth: panel.fittedContentWidth(Style.space(360))
    contentHeight: panel.fittedContentHeight(Style.space(420))

    PanelKeyCatcher {
      id: keyCatcher
      anchors.fill: parent
      onCloseRequested: root.close()
      onTabRequested: function(direction) { root.switchPanel(direction) }

      SurgeCore.SurgePanelContent {
        anchors.fill: parent
        client: surgeClient
        backgroundColor: Color.background
        foregroundColor: root.bar ? root.bar.foreground : Color.foreground
        accentColor: Color.accent
        urgentColor: root.bar ? root.bar.urgent : Color.urgent
        fontFamily: root.bar ? root.bar.fontFamily : Style.font.family
      }
    }
  }
}
