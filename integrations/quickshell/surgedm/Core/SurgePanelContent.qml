import QtQuick
import "SurgeModel.js" as Model

Rectangle {
  id: root

  required property var client
  property color backgroundColor: "#17191f"
  property color foregroundColor: "#f3f4f6"
  property color accentColor: "#63d8ff"
  property color urgentColor: "#ff6b7a"
  property string fontFamily: "sans-serif"

  readonly property color mutedColor: Qt.rgba(foregroundColor.r, foregroundColor.g, foregroundColor.b, 0.62)
  readonly property color faintColor: Qt.rgba(foregroundColor.r, foregroundColor.g, foregroundColor.b, 0.08)
  readonly property color borderColor: Qt.rgba(foregroundColor.r, foregroundColor.g, foregroundColor.b, 0.14)
  readonly property color warningColor: "#f2bd4b"

  function statusColor(status) {
    var value = String(status || "").toLowerCase()
    if (value === "failed" || value === "error") return root.urgentColor
    if (value === "paused" || value === "pausing") return root.warningColor
    if (value === "completed" || Model.isActive(value)) return root.accentColor
    return root.mutedColor
  }

  function summaryText() {
    if (!root.client.available) return root.client.loading ? "Connecting…" : "Offline"
    if (root.client.summary.active > 0)
      return root.client.summary.active + " active · " + Model.formatSpeed(root.client.summary.speed)
    if (root.client.summary.paused > 0)
      return root.client.summary.paused + " paused · " + root.client.summary.total + " total"
    return root.client.summary.total + (root.client.summary.total === 1 ? " download" : " downloads")
  }

  function statusGlyph(status) {
    var value = String(status || "").toLowerCase()
    if (value === "failed" || value === "error") return "!"
    if (value === "paused" || value === "pausing") return "Ⅱ"
    if (value === "completed") return "✓"
    return "↓"
  }

  color: backgroundColor
  radius: 14
  implicitWidth: 360
  implicitHeight: 420
  clip: true

  component ActionButton: Rectangle {
    id: actionButton
    required property string label
    property bool enabled: true
    property bool primary: false
    property bool destructive: false
    signal clicked()

    implicitWidth: Math.max(46, buttonLabel.implicitWidth + 14)
    implicitHeight: 24
    radius: 6
    color: {
      if (!enabled) return root.faintColor
      if (buttonMouse.pressed)
        return Qt.rgba(root.foregroundColor.r, root.foregroundColor.g, root.foregroundColor.b, 0.18)
      if (destructive && buttonMouse.containsMouse)
        return Qt.rgba(root.urgentColor.r, root.urgentColor.g, root.urgentColor.b, 0.16)
      if (buttonMouse.containsMouse)
        return Qt.rgba(root.foregroundColor.r, root.foregroundColor.g, root.foregroundColor.b, 0.13)
      if (primary)
        return Qt.rgba(root.accentColor.r, root.accentColor.g, root.accentColor.b, 0.18)
      if (destructive)
        return Qt.rgba(root.urgentColor.r, root.urgentColor.g, root.urgentColor.b, 0.08)
      return root.faintColor
    }
    border.width: 1
    border.color: destructive
      ? Qt.rgba(root.urgentColor.r, root.urgentColor.g, root.urgentColor.b, 0.3)
      : (primary ? Qt.rgba(root.accentColor.r, root.accentColor.g, root.accentColor.b, 0.45) : root.borderColor)
    opacity: enabled ? 1 : 0.45

    Text {
      id: buttonLabel
      anchors.centerIn: parent
      text: actionButton.label
      color: actionButton.destructive
        ? root.urgentColor
        : (actionButton.primary ? root.accentColor : root.foregroundColor)
      font.family: root.fontFamily
      font.pixelSize: 10
      font.weight: Font.Medium
    }

    MouseArea {
      id: buttonMouse
      anchors.fill: parent
      enabled: actionButton.enabled
      hoverEnabled: true
      cursorShape: Qt.PointingHandCursor
      onClicked: actionButton.clicked()
    }
  }

  Column {
    anchors.fill: parent
    anchors.margins: 11
    spacing: 8

    Item {
      width: parent.width
      height: 38

      Rectangle {
        id: serviceDot
        anchors.left: parent.left
        anchors.verticalCenter: parent.verticalCenter
        width: 7
        height: 7
        radius: 4
        color: root.client.available ? root.accentColor : root.urgentColor
        opacity: root.client.loading ? 0.55 : 1
      }

      Row {
        id: headerActions
        anchors.right: parent.right
        anchors.verticalCenter: parent.verticalCenter
        spacing: 5

        ActionButton {
          label: "Refresh"
          enabled: !root.client.loading
          onClicked: root.client.refresh()
        }

        ActionButton {
          visible: root.client.serviceControlsEnabled
          label: root.client.available ? "Stop" : "Start"
          destructive: root.client.available
          enabled: !root.client.actionRunning
          onClicked: root.client.serviceAction(root.client.available ? "stop" : "start")
        }
      }

      Column {
        anchors.left: serviceDot.right
        anchors.right: headerActions.left
        anchors.leftMargin: 7
        anchors.rightMargin: 8
        anchors.verticalCenter: parent.verticalCenter
        spacing: 1
        clip: true

        Text {
          width: parent.width
          text: "SURGEDM"
          color: root.foregroundColor
          font.family: root.fontFamily
          font.pixelSize: 11
          font.weight: Font.DemiBold
          elide: Text.ElideRight
        }

        Text {
          width: parent.width
          text: root.summaryText()
          color: root.client.available ? root.mutedColor : root.urgentColor
          font.family: root.fontFamily
          font.pixelSize: 9
          elide: Text.ElideRight
        }
      }
    }

    Rectangle {
      width: parent.width
      height: 34
      radius: 7
      color: root.faintColor
      border.width: 1
      border.color: addInput.activeFocus ? root.accentColor : root.borderColor

      Text {
        id: addIcon
        anchors.left: parent.left
        anchors.leftMargin: 9
        anchors.verticalCenter: parent.verticalCenter
        text: "↓"
        color: addInput.activeFocus ? root.accentColor : root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 12
        font.weight: Font.Medium
      }

      TextInput {
        id: addInput
        anchors.left: addIcon.right
        anchors.right: addButton.left
        anchors.leftMargin: 7
        anchors.rightMargin: 6
        anchors.verticalCenter: parent.verticalCenter
        color: root.foregroundColor
        selectionColor: root.accentColor
        selectedTextColor: root.backgroundColor
        font.family: root.fontFamily
        font.pixelSize: 10
        clip: true
        selectByMouse: true
        onAccepted: {
          if (root.client.add(text)) text = ""
        }

        Text {
          anchors.fill: parent
          visible: addInput.text === ""
          text: "Paste a download URL"
          color: root.mutedColor
          font: addInput.font
          verticalAlignment: Text.AlignVCenter
        }
      }

      ActionButton {
        id: addButton
        anchors.right: parent.right
        anchors.rightMargin: 4
        anchors.verticalCenter: parent.verticalCenter
        label: "Add"
        primary: true
        enabled: !root.client.actionRunning && addInput.text.trim() !== ""
        onClicked: {
          if (root.client.add(addInput.text)) addInput.text = ""
        }
      }
    }

    Rectangle {
      visible: root.client.errorText !== "" || root.client.actionMessage !== ""
      width: parent.width
      height: messageText.implicitHeight + 12
      radius: 6
      color: root.client.errorText !== ""
        ? Qt.rgba(root.urgentColor.r, root.urgentColor.g, root.urgentColor.b, 0.12)
        : root.faintColor
      border.width: 1
      border.color: root.client.errorText !== ""
        ? Qt.rgba(root.urgentColor.r, root.urgentColor.g, root.urgentColor.b, 0.32)
        : root.borderColor

      Text {
        id: messageText
        anchors.fill: parent
        anchors.margins: 6
        text: root.client.errorText || root.client.actionMessage
        color: root.client.errorText !== "" ? root.urgentColor : root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 9
        wrapMode: Text.Wrap
      }
    }

    Item {
      visible: root.client.available
      width: parent.width
      height: visible ? 18 : 0

      Text {
        anchors.left: parent.left
        anchors.verticalCenter: parent.verticalCenter
        text: "DOWNLOADS"
        color: root.foregroundColor
        font.family: root.fontFamily
        font.pixelSize: 10
        font.weight: Font.DemiBold
      }

      Text {
        anchors.right: parent.right
        anchors.verticalCenter: parent.verticalCenter
        text: root.client.summary.total + " total"
        color: root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 9
      }
    }

    Item {
      width: parent.width
      height: parent.height - y

      Text {
        anchors.centerIn: parent
        visible: root.client.available && root.client.downloads.length === 0
        width: parent.width
        text: "No downloads yet.\nPaste a URL above to get started."
        color: root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 10
        horizontalAlignment: Text.AlignHCenter
        wrapMode: Text.WordWrap
      }

      Text {
        anchors.centerIn: parent
        visible: !root.client.available && !root.client.loading
        width: parent.width - 36
        horizontalAlignment: Text.AlignHCenter
        wrapMode: Text.Wrap
        text: root.client.serviceControlsEnabled
          ? "Start the user service to connect securely."
          : "Check the Surge host and authentication settings."
        color: root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 10
      }

      Text {
        anchors.centerIn: parent
        visible: root.client.loading && !root.client.available
        text: "Connecting…"
        color: root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 10
      }

      ListView {
        id: downloadList
        anchors.fill: parent
        visible: root.client.available && root.client.downloads.length > 0
        clip: true
        spacing: 6
        model: root.client.downloads
        boundsBehavior: Flickable.StopAtBounds

        delegate: Rectangle {
          id: row
          required property var modelData
          readonly property color stateColor: root.statusColor(modelData.status)
          width: downloadList.width
          height: 82
          radius: 7
          color: rowHover.containsMouse
            ? Qt.rgba(root.foregroundColor.r, root.foregroundColor.g, root.foregroundColor.b, 0.1)
            : root.faintColor
          border.width: 1
          border.color: rowHover.containsMouse
            ? Qt.rgba(root.foregroundColor.r, root.foregroundColor.g, root.foregroundColor.b, 0.18)
            : root.borderColor

          MouseArea {
            id: rowHover
            anchors.fill: parent
            acceptedButtons: Qt.NoButton
            hoverEnabled: true
          }

          Column {
            anchors.fill: parent
            anchors.margins: 7
            spacing: 4

            Row {
              width: parent.width
              height: 27
              spacing: 7

              Text {
                anchors.verticalCenter: parent.verticalCenter
                width: 18
                text: root.statusGlyph(row.modelData.status)
                color: row.stateColor
                font.family: root.fontFamily
                font.pixelSize: 14
                font.weight: Font.DemiBold
                horizontalAlignment: Text.AlignHCenter
              }

              Column {
                width: parent.width - 25
                spacing: 1
                clip: true

                Text {
                  width: parent.width
                  text: row.modelData.filename
                  color: root.foregroundColor
                  font.family: root.fontFamily
                  font.pixelSize: 10
                  font.weight: Font.DemiBold
                  elide: Text.ElideMiddle
                }

                Text {
                  width: parent.width
                  text: Model.statusLabel(row.modelData.status)
                    + "  ·  " + Math.round(row.modelData.progress) + "%"
                    + (row.modelData.speed > 0 ? "  ·  " + Model.formatSpeed(row.modelData.speed) : "")
                  color: row.stateColor
                  font.family: root.fontFamily
                  font.pixelSize: 9
                  elide: Text.ElideRight
                }
              }
            }

            Rectangle {
              width: parent.width
              height: 3
              radius: 2
              color: root.borderColor

              Rectangle {
                width: parent.width * Math.max(0, Math.min(100, row.modelData.progress)) / 100
                height: parent.height
                radius: parent.radius
                color: row.stateColor
                Behavior on width {
                  NumberAnimation { duration: 180; easing.type: Easing.OutCubic }
                }
              }
            }

            Row {
              spacing: 5

              ActionButton {
                visible: row.modelData.status === "paused" || Model.isActive(row.modelData.status)
                label: row.modelData.status === "paused" ? "Start" : "Stop"
                primary: row.modelData.status === "paused"
                enabled: !root.client.actionRunning
                onClicked: {
                  if (row.modelData.status === "paused") root.client.resume(row.modelData.id)
                  else root.client.pause(row.modelData.id)
                }
              }

              ActionButton {
                label: "Delete"
                destructive: true
                enabled: !root.client.actionRunning
                onClicked: root.client.removeDownload(row.modelData.id)
              }
            }
          }
        }
      }
    }
  }
}
