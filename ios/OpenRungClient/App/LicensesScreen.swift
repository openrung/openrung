import SwiftUI

/// In-app open-source licenses: the GPL-3.0 notice, the corresponding-source link, the per-component
/// license list, and the full bundled notices text (GNU GPL-3.0). Mirrors the Android licenses screen.
struct LicensesScreen: View {
    let onBack: () -> Void

    @State private var showFullText = false
    @Environment(\.openURL) private var openURL

    private struct Entry: Identifiable {
        let id = UUID()
        let name: String
        let license: String
    }

    // Components distributed in the iOS app (see THIRD_PARTY_NOTICES.md). SwiftUI is an Apple system
    // framework and OpenRungKit is first-party (GPL-3.0 under the repo license).
    private let entries: [Entry] = [
        Entry(name: "sing-box (libbox)", license: "GPL-3.0-or-later"),
        Entry(name: "gVisor", license: "Apache-2.0"),
        Entry(name: "quic-go", license: "MIT"),
        Entry(name: "wireguard-go", license: "MIT"),
        Entry(name: "utls", license: "BSD-3-Clause"),
        Entry(name: "Go x/crypto, x/sync, x/text + stdlib", license: "BSD-3-Clause"),
        Entry(name: "OpenRungKit (first-party)", license: "GPL-3.0-or-later"),
    ]

    var body: some View {
        if showFullText {
            LicenseTextScreen(onBack: { showFullText = false })
        } else {
            list
        }
    }

    private var list: some View {
        ZStack {
            Theme.screen.ignoresSafeArea()

            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    LicensesHeader(title: Strings.licensesTitle, onBack: onBack)

                    Text(Strings.licensesIntro)
                        .font(Theme.mono(13))
                        .foregroundColor(Theme.statusText)
                        .fixedSize(horizontal: false, vertical: true)

                    TappablePanel(title: Strings.licensesSourceTitle, subtitle: Strings.sourceURL) {
                        if let url = URL(string: Strings.sourceURL) { openURL(url) }
                    }

                    TappablePanel(title: Strings.licensesFullTextTitle, subtitle: Strings.licensesFullTextSubtitle) {
                        showFullText = true
                    }

                    Text(Strings.licensesComponentsHeader)
                        .font(Theme.mono(14, weight: .bold))
                        .foregroundColor(Theme.terminalGreen)
                        .padding(.top, 4)

                    ForEach(entries) { entry in
                        SettingPanel(title: entry.name, subtitle: entry.license)
                    }
                }
                .padding(20)
            }
        }
    }
}

/// Full bundled notices (component summary + complete GNU GPL-3.0 text), read from the app bundle.
private struct LicenseTextScreen: View {
    let onBack: () -> Void

    private var notices: String {
        guard let url = Bundle.main.url(forResource: "third_party_notices", withExtension: "txt"),
              let text = try? String(contentsOf: url, encoding: .utf8)
        else {
            return "License notices unavailable."
        }
        return text
    }

    var body: some View {
        ZStack {
            Theme.screen.ignoresSafeArea()

            VStack(alignment: .leading, spacing: 16) {
                LicensesHeader(title: Strings.licensesFullTextTitle, onBack: onBack)

                ScrollView {
                    Text(notices)
                        .font(Theme.mono(10))
                        .foregroundColor(Theme.statusText)
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
            }
            .padding(20)
        }
    }
}

/// Back arrow + title row, matching the Settings header.
private struct LicensesHeader: View {
    let title: String
    let onBack: () -> Void

    var body: some View {
        HStack(spacing: 8) {
            Button(action: onBack) {
                Image(systemName: "chevron.backward")
                    .font(.system(size: 18, weight: .semibold))
                    .foregroundColor(Theme.terminalGreen)
            }
            Text(title)
                .font(Theme.mono(22, weight: .bold))
                .foregroundColor(Theme.terminalGreen)
        }
    }
}

/// A `SettingPanel` made tappable, with a trailing chevron. Used for navigation rows.
struct TappablePanel: View {
    let title: String
    let subtitle: String
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            SettingPanel(title: title, subtitle: subtitle) {
                Image(systemName: "chevron.forward")
                    .font(.system(size: 14, weight: .semibold))
                    .foregroundColor(Theme.terminalGreen)
            }
        }
        .buttonStyle(.plain)
    }
}
