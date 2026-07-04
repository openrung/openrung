import SwiftUI

extension Color {
    init(hex: UInt32) {
        self.init(
            .sRGB,
            red: Double((hex >> 16) & 0xFF) / 255.0,
            green: Double((hex >> 8) & 0xFF) / 255.0,
            blue: Double(hex & 0xFF) / 255.0,
            opacity: 1.0
        )
    }
}

/// The terminal aesthetic shared with the Android app (exact hex palette from `MainActivity`).
enum Theme {
    static let screen = Color(hex: 0x030604)
    static let terminalGreen = Color(hex: 0x65F58A)
    static let activeButton = Color(hex: 0xB6F579)
    static let statusText = Color(hex: 0xD8FFE0)
    static let relayText = Color(hex: 0xA5F2B5)
    static let subtitle = Color(hex: 0x7DA989)
    static let dimGreen = Color(hex: 0x294F35)
    static let panelBlack = Color(hex: 0x07110B)
    static let errorText = Color(hex: 0xFFA0A0)
    static let buttonContent = Color(hex: 0x061008)
    static let fabContainer = Color(hex: 0x0D1C12)

    static func mono(_ size: CGFloat, weight: Font.Weight = .regular) -> Font {
        .system(size: size, weight: weight, design: .monospaced)
    }
}

/// A bordered terminal panel with a title/subtitle and an optional trailing control.
/// Port of the Android `SettingPanel` composable.
struct SettingPanel<Trailing: View>: View {
    let title: String
    let subtitle: String
    @ViewBuilder var trailing: () -> Trailing

    var body: some View {
        HStack(alignment: .center, spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                Text(title)
                    .font(Theme.mono(15, weight: .bold))
                    .foregroundColor(Theme.statusText)
                Text(subtitle)
                    .font(Theme.mono(13))
                    .foregroundColor(Theme.subtitle)
                    .fixedSize(horizontal: false, vertical: true)
            }
            .frame(maxWidth: .infinity, alignment: .leading)
            trailing()
        }
        .padding(14)
        .background(Theme.panelBlack)
        .clipShape(RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).stroke(Theme.dimGreen, lineWidth: 1))
    }
}

extension SettingPanel where Trailing == EmptyView {
    init(title: String, subtitle: String) {
        self.init(title: title, subtitle: subtitle) { EmptyView() }
    }
}
