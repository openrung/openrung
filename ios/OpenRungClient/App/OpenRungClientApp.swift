import SwiftUI

@main
struct OpenRungClientApp: App {
    @StateObject private var viewModel = AppViewModel()

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(viewModel)
                .task {
                    await viewModel.load()
                }
        }
    }
}
