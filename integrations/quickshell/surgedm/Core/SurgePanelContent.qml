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

  color: backgroundColor
  radius: 14
  implicitWidth: 390
  implicitHeight: 470
  clip: true

  component ActionButton: Rectangle {
    id: actionButton
    required property string label
    property bool enabled: true
    property bool primary: false
    property bool destructive: false
    signal clicked()

    implicitWidth: Math.max(62, buttonLabel.implicitWidth + 22)
    implicitHeight: 30
    radius: 8
    color: {
      if (!enabled) return root.faintColor
      if (buttonMouse.pressed) return Qt.rgba(root.foregroundColor.r, root.foregroundColor.g, root.foregroundColor.b, 0.18)
      if (buttonMouse.containsMouse) return Qt.rgba(root.foregroundColor.r, root.foregroundColor.g, root.foregroundColor.b, 0.13)
      return primary ? Qt.rgba(root.accentColor.r, root.accentColor.g, root.accentColor.b, 0.18) : root.faintColor
    }
    border.width: 1
    border.color: primary ? Qt.rgba(root.accentColor.r, root.accentColor.g, root.accentColor.b, 0.45) : root.borderColor
    opacity: enabled ? 1 : 0.45

    Text {
      id: buttonLabel
      anchors.centerIn: parent
      text: actionButton.label
      color: actionButton.destructive ? root.urgentColor : (actionButton.primary ? root.accentColor : root.foregroundColor)
      font.family: root.fontFamily
      font.pixelSize: 12
      font.weight: Font.DemiBold
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
    anchors.margins: 16
    spacing: 12

    Item {
      width: parent.width
      height: 42

      Column {
        anchors.left: parent.left
        anchors.verticalCenter: parent.verticalCenter
        spacing: 2

        Text {
          text: "SurgeDM"
          color: root.foregroundColor
          font.family: root.fontFamily
          font.pixelSize: 18
          font.weight: Font.DemiBold
        }

        Text {
          text: root.client.available
            ? (root.client.summary.active > 0
                ? root.client.summary.active + " active · " + Model.formatSpeed(root.client.summary.speed)
                : "Connected · ready")
            : (root.client.loading ? "Connecting…" : "Offline")
          color: root.client.available ? root.accentColor : root.mutedColor
          font.family: root.fontFamily
          font.pixelSize: 11
        }
      }

      Row {
        anchors.right: parent.right
        anchors.verticalCenter: parent.verticalCenter
        spacing: 7

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
    }

    Rectangle {
      width: parent.width
      height: 38
      radius: 9
      color: root.faintColor
      border.width: 1
      border.color: addInput.activeFocus ? root.accentColor : root.borderColor

      TextInput {
        id: addInput
        anchors.left: parent.left
        anchors.right: addButton.left
        anchors.leftMargin: 12
        anchors.rightMargin: 8
        anchors.verticalCenter: parent.verticalCenter
        color: root.foregroundColor
        selectionColor: root.accentColor
        selectedTextColor: root.backgroundColor
        font.family: root.fontFamily
        font.pixelSize: 12
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
        enabled: !root.client.actionRunning
        onClicked: {
          if (root.client.add(addInput.text)) addInput.text = ""
        }
      }
    }

    Rectangle {
      visible: root.client.errorText !== "" || root.client.actionMessage !== ""
      width: parent.width
      height: messageText.implicitHeight + 16
      radius: 8
      color: root.client.errorText !== ""
        ? Qt.rgba(root.urgentColor.r, root.urgentColor.g, root.urgentColor.b, 0.12)
        : root.faintColor

      Text {
        id: messageText
        anchors.fill: parent
        anchors.margins: 8
        text: root.client.errorText || root.client.actionMessage
        color: root.client.errorText !== "" ? root.urgentColor : root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 11
        wrapMode: Text.Wrap
      }
    }

    Item {
      width: parent.width
      height: parent.height - y

      Text {
        anchors.centerIn: parent
        visible: root.client.available && root.client.downloads.length === 0
        text: "No downloads yet"
        color: root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 13
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
        font.pixelSize: 13
      }

      Text {
        anchors.centerIn: parent
        visible: root.client.loading && !root.client.available
        text: "Connecting…"
        color: root.mutedColor
        font.family: root.fontFamily
        font.pixelSize: 13
      }

      ListView {
        id: downloadList
        anchors.fill: parent
        visible: root.client.available && root.client.downloads.length > 0
        clip: true
        spacing: 7
        model: root.client.downloads
        boundsBehavior: Flickable.StopAtBounds

        delegate: Rectangle {
          id: row
          required property var modelData
          width: downloadList.width
          height: 64
          radius: 9
          color: root.faintColor
          border.width: 1
          border.color: root.borderColor

          Column {
            anchors.left: parent.left
            anchors.right: rowAction.left
            anchors.top: parent.top
            anchors.leftMargin: 11
            anchors.rightMargin: 10
            anchors.topMargin: 9
            spacing: 5

            Text {
              width: parent.width
              text: row.modelData.filename
              color: root.foregroundColor
              font.family: root.fontFamily
              font.pixelSize: 12
              font.weight: Font.Medium
              elide: Text.ElideMiddle
            }

            Row {
              spacing: 8
              Text {
                text: row.modelData.status
                color: row.modelData.status === "failed" ? root.urgentColor : root.mutedColor
                font.family: root.fontFamily
                font.pixelSize: 10
              }
              Text {
                text: Math.round(row.modelData.progress) + "%"
                color: root.mutedColor
                font.family: root.fontFamily
                font.pixelSize: 10
              }
              Text {
                visible: row.modelData.speed > 0
                text: Model.formatSpeed(row.modelData.speed)
                color: root.accentColor
                font.family: root.fontFamily
                font.pixelSize: 10
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
                radius: 2
                color: root.accentColor
              }
            }
          }

          ActionButton {
            id: rowAction
            anchors.right: parent.right
            anchors.rightMargin: 9
            anchors.verticalCenter: parent.verticalCenter
            visible: row.modelData.status === "paused" || Model.isActive(row.modelData.status)
            label: row.modelData.status === "paused" ? "Resume" : "Pause"
            enabled: !root.client.actionRunning
            onClicked: {
              if (row.modelData.status === "paused") root.client.resume(row.modelData.id)
              else root.client.pause(row.modelData.id)
            }
          }
        }
      }
    }
  }
}
