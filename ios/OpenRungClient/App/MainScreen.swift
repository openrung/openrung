import MapKit
import OpenRungKit
import SwiftUI

/// The terminal main screen: title, status, relay label, connect/disconnect, selectable exit-node
/// map, recent locations, traffic-route footer, and the settings FAB.
struct MainScreen: View {
    @ObservedObject var viewModel: AppViewModel
    let onOpenSettings: () -> Void

    var body: some View {
        ZStack(alignment: .bottomTrailing) {
            Theme.screen.ignoresSafeArea()

            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    Text(Strings.mainTitle)
                        .font(Theme.mono(22, weight: .bold))
                        .foregroundColor(Theme.terminalGreen)

                    Text(Strings.status(viewModel.status.displayLabel))
                        .font(Theme.mono(13))
                        .foregroundColor(Theme.statusText)

                    if let relay = viewModel.relayLabel {
                        Text(Strings.relay(relay))
                            .font(Theme.mono(13))
                            .foregroundColor(Theme.relayText)
                    }

                    connectButton
                    mapPanel
                    recentsSection

                    Text(viewModel.isConnected ? Strings.trafficRouteConnected : Strings.trafficRouteDisconnected)
                        .font(Theme.mono(12))
                        .foregroundColor(Theme.subtitle)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .padding(.horizontal, 20)
                .padding(.top, 20)
                .padding(.bottom, 104)
            }
            .scrollIndicators(.hidden)
            .task { viewModel.refreshDirectory() }

            settingsButton
        }
    }

    private var connectButton: some View {
        Button {
            Task { await viewModel.toggle() }
        } label: {
            Text(viewModel.isActive ? Strings.actionDisconnect : Strings.actionConnect)
                .font(Theme.mono(15, weight: .black))
                .tracking(1)
                .frame(maxWidth: .infinity)
                .frame(height: 58)
                .background(viewModel.isActive ? Theme.activeButton : Theme.terminalGreen)
                .foregroundColor(Theme.buttonContent)
                .clipShape(RoundedRectangle(cornerRadius: 8))
        }
        .buttonStyle(.plain)
    }

    private var mapPanel: some View {
        ZStack(alignment: .topLeading) {
            ExitNodeMap(regions: viewModel.availableRegions) { countryCode in
                Task { await viewModel.connect(countryCode: countryCode) }
            }

            MapStatusChip(
                status: viewModel.directoryStatus,
                regionCount: viewModel.availableRegions.count,
                onRetry: { viewModel.refreshDirectory(force: true) }
            )
            .padding(10)
        }
        .frame(height: 320)
        .background(Theme.panelBlack)
        .clipShape(RoundedRectangle(cornerRadius: 12))
        .overlay(RoundedRectangle(cornerRadius: 12).stroke(Theme.dimGreen, lineWidth: 1))
    }

    private var recentsSection: some View {
        VStack(alignment: .leading, spacing: 10) {
            Text(Strings.recentsLabel)
                .font(Theme.mono(14, weight: .bold))
                .foregroundColor(Theme.statusText)

            if viewModel.recentRegions.isEmpty {
                Text(Strings.recentsEmpty)
                    .font(Theme.mono(12))
                    .foregroundColor(Theme.subtitle)
            } else {
                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 10) {
                        ForEach(viewModel.recentRegions) { node in
                            RecentNodeCard(node: node)
                        }
                    }
                    .padding(.vertical, 1)
                }
            }
        }
    }

    private var settingsButton: some View {
        Button(action: onOpenSettings) {
            Image(systemName: "gearshape")
                .font(.system(size: 20))
                .foregroundColor(Theme.terminalGreen)
                .frame(width: 56, height: 56)
                .background(Theme.fabContainer)
                .clipShape(Circle())
        }
        .padding(20)
    }
}

private struct ExitNodeMap: View {
    let regions: [ExitNodeRegion]
    let onSelect: (String) -> Void

    @State private var cameraRegion = MKCoordinateRegion(
        center: CLLocationCoordinate2D(latitude: 18.0, longitude: 116.0),
        span: MKCoordinateSpan(latitudeDelta: 62.0, longitudeDelta: 88.0)
    )

    var body: some View {
        Map(
            coordinateRegion: $cameraRegion,
            interactionModes: [],
            annotationItems: regions
        ) { node in
            MapAnnotation(coordinate: CLLocationCoordinate2D(latitude: node.latitude, longitude: node.longitude)) {
                Button {
                    onSelect(node.countryCode)
                } label: {
                    NodeMarker(count: node.nodeCount)
                }
                .buttonStyle(.plain)
                .accessibilityLabel(node.countryName)
            }
        }
        .colorScheme(.dark)
        .saturation(0.35)
        .brightness(-0.18)
        .overlay(Theme.screen.opacity(0.18).allowsHitTesting(false))
    }
}

private struct NodeMarker: View {
    let count: Int

    var body: some View {
        ZStack {
            Circle()
                .fill(Theme.terminalGreen.opacity(0.18))
                .frame(width: 44, height: 44)
            Circle()
                .fill(Theme.terminalGreen)
                .frame(width: 15, height: 15)
                .overlay(Circle().stroke(Theme.screen, lineWidth: 2))
            Text("\(count)")
                .font(Theme.mono(11, weight: .bold))
                .foregroundColor(Theme.terminalGreen)
                .shadow(color: Theme.screen, radius: 1)
                .offset(y: -23)
        }
        .contentShape(Circle())
    }
}

private struct MapStatusChip: View {
    let status: DirectoryStatus
    let regionCount: Int
    let onRetry: () -> Void

    private var canRetry: Bool {
        status == .failed || (status == .loaded && regionCount == 0)
    }

    private var label: String {
        switch status {
        case .idle, .loading:
            return Strings.mapLoading
        case .failed:
            return Strings.mapFailed
        case .loaded where regionCount == 0:
            return Strings.mapNoNodes
        case .loaded:
            return Strings.mapNodesAvailable(regionCount)
        }
    }

    var body: some View {
        Button {
            if canRetry { onRetry() }
        } label: {
            Text(label)
                .font(Theme.mono(12))
                .foregroundColor(status == .failed ? Theme.errorText : Theme.statusText)
                .padding(.horizontal, 10)
                .padding(.vertical, 6)
                .background(Theme.panelBlack.opacity(0.86))
                .clipShape(RoundedRectangle(cornerRadius: 6))
        }
        .buttonStyle(.plain)
        .allowsHitTesting(canRetry)
    }
}

private struct RecentNodeCard: View {
    let node: RecentNode

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(countryFlag(node.countryCode))
                .font(.system(size: 22))
            Text(node.label)
                .font(Theme.mono(13))
                .foregroundColor(Theme.statusText)
                .lineLimit(2)
                .fixedSize(horizontal: false, vertical: true)
        }
        .frame(width: 140, alignment: .topLeading)
        .frame(minHeight: 86, alignment: .topLeading)
        .padding(12)
        .background(Theme.panelBlack)
        .clipShape(RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).stroke(Theme.dimGreen, lineWidth: 1))
    }
}

private func countryFlag(_ code: String) -> String {
    let scalars = code.trimmingCharacters(in: .whitespacesAndNewlines).uppercased().unicodeScalars
    guard scalars.count == 2,
          scalars.allSatisfy({ (65...90).contains(Int($0.value)) })
    else {
        return "--"
    }
    let flagScalars = scalars.compactMap { UnicodeScalar(127397 + Int($0.value)) }
    return String(String.UnicodeScalarView(flagScalars))
}
