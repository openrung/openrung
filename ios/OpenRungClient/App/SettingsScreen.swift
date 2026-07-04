import SwiftUI
import OpenRungKit

/// Settings: the volunteer speed test (runs only while connected) and the app version.
/// Port of Android `OpenRungSettingsScreen` (minus the language picker, per the iOS plan).
struct SettingsScreen: View {
    @ObservedObject var viewModel: AppViewModel
    let onBack: () -> Void

    @State private var speedTestRunning = false
    @State private var speedTestResult: SpeedTestResult?
    @State private var speedTestError: String?
    @State private var showLicenses = false
    @State private var showDebug = false

    var body: some View {
        if showDebug {
            DebugScreen(viewModel: viewModel, onBack: { showDebug = false })
        } else if showLicenses {
            LicensesScreen(onBack: { showLicenses = false })
        } else {
            settingsContent
        }
    }

    private var settingsContent: some View {
        ZStack {
            Theme.screen.ignoresSafeArea()

            ScrollView {
                VStack(alignment: .leading, spacing: 18) {
                    HStack(spacing: 8) {
                        Button(action: onBack) {
                            Image(systemName: "chevron.backward")
                                .font(.system(size: 18, weight: .semibold))
                                .foregroundColor(Theme.terminalGreen)
                        }
                        Text(Strings.settingsTitle)
                            .font(Theme.mono(22, weight: .bold))
                            .foregroundColor(Theme.terminalGreen)
                    }

                    speedTestPanel
                    SettingPanel(title: Strings.versionTitle, subtitle: DeviceAttributes.appVersion)
                    TappablePanel(
                        title: Strings.debugSettingTitle,
                        subtitle: Strings.debugSettingSubtitle
                    ) {
                        showDebug = true
                    }
                    TappablePanel(
                        title: Strings.licensesSettingTitle,
                        subtitle: Strings.licensesSettingSubtitle
                    ) {
                        showLicenses = true
                    }
                }
                .padding(20)
            }
        }
    }

    private var speedTestSubtitle: String {
        if speedTestRunning { return Strings.speedTestRunning }
        if let error = speedTestError { return Strings.speedTestError(error) }
        if let result = speedTestResult { return Strings.speedTestResult(result.downloadMbps) }
        if viewModel.isConnected == false { return Strings.speedTestRequiresConnection }
        return Strings.speedTestReady
    }

    private var speedTestPanel: some View {
        SettingPanel(title: Strings.speedTestTitle, subtitle: speedTestSubtitle) {
            Button(action: runSpeedTest) {
                Text(Strings.speedTestAction)
                    .font(Theme.mono(14, weight: .bold))
                    .padding(.horizontal, 16)
                    .padding(.vertical, 10)
                    .background(Theme.terminalGreen)
                    .foregroundColor(Theme.buttonContent)
                    .clipShape(RoundedRectangle(cornerRadius: 8))
            }
            .buttonStyle(.plain)
            .disabled(viewModel.isConnected == false || speedTestRunning)
            .opacity((viewModel.isConnected == false || speedTestRunning) ? 0.5 : 1)
        }
    }

    private func runSpeedTest() {
        speedTestRunning = true
        speedTestResult = nil
        speedTestError = nil
        Task {
            let outcome = await viewModel.runSpeedTest()
            switch outcome {
            case .success(let result):
                speedTestResult = result
            case .failure(let error):
                speedTestError = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
            }
            speedTestRunning = false
        }
    }
}

private struct DebugScreen: View {
    @ObservedObject var viewModel: AppViewModel
    let onBack: () -> Void

    var body: some View {
        ZStack {
            Theme.screen.ignoresSafeArea()

            VStack(alignment: .leading, spacing: 16) {
                HStack(spacing: 8) {
                    Button(action: onBack) {
                        Image(systemName: "chevron.backward")
                            .font(.system(size: 18, weight: .semibold))
                            .foregroundColor(Theme.terminalGreen)
                    }
                    Text(Strings.debugTitle)
                        .font(Theme.mono(22, weight: .bold))
                        .foregroundColor(Theme.terminalGreen)
                }

                ConsolePanel(viewModel: viewModel)

                Text(viewModel.isConnected ? Strings.trafficRouteConnected : Strings.trafficRouteDisconnected)
                    .font(Theme.mono(12))
                    .foregroundColor(Theme.subtitle)
                    .frame(maxWidth: .infinity, alignment: .leading)
            }
            .padding(20)
        }
    }
}

private struct ConsolePanel: View {
    @ObservedObject var viewModel: AppViewModel

    var body: some View {
        let lines = viewModel.logLines.isEmpty ? [Strings.readyLog] : viewModel.logLines
        ScrollViewReader { proxy in
            ScrollView {
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(Array(lines.enumerated()), id: \.offset) { index, line in
                        Text(Strings.logLine(line))
                            .font(Theme.mono(13))
                            .foregroundColor(Theme.terminalGreen)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .id("line-\(index)")
                    }
                    if let error = viewModel.lastError {
                        Text(Strings.errorLine(error))
                            .font(Theme.mono(13))
                            .foregroundColor(Theme.errorText)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding(.top, 8)
                            .id("error")
                    }
                }
                .padding(14)
                .frame(maxWidth: .infinity, alignment: .leading)
            }
            .background(Theme.panelBlack)
            .clipShape(RoundedRectangle(cornerRadius: 8))
            .overlay(RoundedRectangle(cornerRadius: 8).stroke(Theme.dimGreen, lineWidth: 1))
            .frame(maxWidth: .infinity, maxHeight: .infinity)
            .onChange(of: viewModel.logLines.count) { _ in
                let target = viewModel.lastError != nil ? "error" : "line-\(max(lines.count - 1, 0))"
                withAnimation { proxy.scrollTo(target, anchor: .bottom) }
            }
        }
    }
}
