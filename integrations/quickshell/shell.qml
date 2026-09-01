import QtQuick
import Quickshell
import "surgedm/Core" as SurgeDM

ShellRoot {
  Variants {
    model: Quickshell.screens

    delegate: Component {
      PanelWindow {
        required property var modelData
        screen: modelData

        anchors {
          top: true
          left: true
          right: true
        }

        implicitHeight: 38
        color: "#111318"

        Text {
          anchors.left: parent.left
          anchors.leftMargin: 14
          anchors.verticalCenter: parent.verticalCenter
          text: "Quickshell"
          color: "#8a909d"
          font.pixelSize: 12
        }

        SurgeDM.SurgeWidget {
          anchors.right: parent.right
          anchors.rightMargin: 8
          anchors.verticalCenter: parent.verticalCenter

          // Optional settings:
          // surgeCommand: "/absolute/path/to/surge"
          // host: "127.0.0.1:1700"
          // downloadDirectory: "/home/me/Downloads"
          // pollInterval: 2000
          // serviceControlsEnabled: true

          backgroundColor: "#17191f"
          foregroundColor: "#f3f4f6"
          accentColor: "#63d8ff"
          urgentColor: "#ff6b7a"
        }
      }
    }
  }
}
