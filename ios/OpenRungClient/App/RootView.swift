import SwiftUI

/// Two-screen navigation (main ⇄ settings), matching the Android route swap so the full-bleed
/// terminal look isn't broken by navigation chrome.
struct RootView: View {
    @EnvironmentObject private var viewModel: AppViewModel
    @State private var showSettings = false

    var body: some View {
        Group {
            if showSettings {
                SettingsScreen(viewModel: viewModel, onBack: { showSettings = false })
            } else {
                MainScreen(viewModel: viewModel, onOpenSettings: { showSettings = true })
            }
        }
        .preferredColorScheme(.dark)
    }
}
