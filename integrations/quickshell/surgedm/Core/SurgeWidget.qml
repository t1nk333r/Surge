import QtQuick
import Quickshell

Item {
  id: root

  property string surgeCommand: "surge"
  property string host: ""
  property string serviceUnit: "surge.service"
  property string downloadDirectory: ""
  property int pollInterval: 2000
  property bool serviceControlsEnabled: true
  property bool popupAbove: false
  property color backgroundColor: "#17191f"
  property color foregroundColor: "#f3f4f6"
  property color accentColor: "#63d8ff"
  property color urgentColor: "#ff6b7a"
  property string fontFamily: "sans-serif"

  readonly property alias client: surgeClient
  readonly property bool opened: popup.visible

  implicitWidth: pill.implicitWidth
  implicitHeight: pill.implicitHeight

  function open() {
    popup.visible = true
    surgeClient.refresh()
  }

  function close() {
    popup.visible = false
  }

  function toggle() {
    if (popup.visible) close()
    else open()
  }

  SurgeClient {
    id: surgeClient
    surgeCommand: root.surgeCommand
    host: root.host
    serviceUnit: root.serviceUnit
    downloadDirectory: root.downloadDirectory
    pollInterval: root.pollInterval
    serviceControlsEnabled: root.serviceControlsEnabled
  }

  Rectangle {
    id: pill
    anchors.fill: parent
    implicitWidth: 30
    implicitHeight: 30
    radius: 9
    color: pillMouse.containsMouse
      ? Qt.rgba(root.foregroundColor.r, root.foregroundColor.g, root.foregroundColor.b, 0.13)
      : Qt.rgba(root.backgroundColor.r, root.backgroundColor.g, root.backgroundColor.b, 0.9)
    border.width: 1
    border.color: surgeClient.available
      ? Qt.rgba(root.accentColor.r, root.accentColor.g, root.accentColor.b, 0.38)
      : Qt.rgba(root.foregroundColor.r, root.foregroundColor.g, root.foregroundColor.b, 0.16)

    Text {
      anchors.centerIn: parent
      text: "󰇚"
      color: root.foregroundColor
      font.family: root.fontFamily
      font.pixelSize: 16
      renderType: Text.NativeRendering
    }

    Rectangle {
      anchors.right: parent.right
      anchors.bottom: parent.bottom
      anchors.rightMargin: 4
      anchors.bottomMargin: 4
      width: 6
      height: 6
      radius: 3
      color: surgeClient.available ? root.accentColor : root.urgentColor
      opacity: surgeClient.loading ? 0.55 : 1
    }

    MouseArea {
      id: pillMouse
      anchors.fill: parent
      hoverEnabled: true
      acceptedButtons: Qt.LeftButton | Qt.MiddleButton
      cursorShape: Qt.PointingHandCursor
      onClicked: function(mouse) {
        if (mouse.button === Qt.MiddleButton) surgeClient.refresh()
        else root.toggle()
      }
    }
  }

  PopupWindow {
    id: popup
    anchor.item: root
    anchor.edges: root.popupAbove ? (Edges.Top | Edges.Right) : (Edges.Bottom | Edges.Right)
    anchor.gravity: root.popupAbove ? (Edges.Top | Edges.Left) : (Edges.Bottom | Edges.Left)
    anchor.margins.top: root.popupAbove ? 0 : 8
    anchor.margins.bottom: root.popupAbove ? 8 : 0
    implicitWidth: 360
    implicitHeight: 420
    color: "transparent"
    grabFocus: true

    SurgePanelContent {
      anchors.fill: parent
      client: surgeClient
      backgroundColor: root.backgroundColor
      foregroundColor: root.foregroundColor
      accentColor: root.accentColor
      urgentColor: root.urgentColor
      fontFamily: root.fontFamily
    }
  }
}
