// swift-tools-version: 5.9

import PackageDescription

let package = Package(
    name: "OpenRungKit",
    platforms: [
        .iOS(.v16),
        .macOS(.v13)
    ],
    products: [
        .library(
            name: "OpenRungKit",
            targets: ["OpenRungKit"]
        )
    ],
    targets: [
        .target(
            name: "OpenRungKit"
        ),
        .testTarget(
            name: "OpenRungKitTests",
            dependencies: ["OpenRungKit"]
        )
    ]
)
