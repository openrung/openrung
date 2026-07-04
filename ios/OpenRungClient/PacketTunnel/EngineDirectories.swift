#if canImport(Libbox)
import Foundation

struct EngineDirectories {
    let base: URL
    let working: URL
    let temporary: URL

    static func make() throws -> EngineDirectories {
        let fileManager = FileManager.default
        let base = try fileManager.url(
            for: .applicationSupportDirectory,
            in: .userDomainMask,
            appropriateFor: nil,
            create: true
        ).appendingPathComponent(AppConfig.engineDirectoryName, isDirectory: true)
        let working = base.appendingPathComponent("Working", isDirectory: true)
        let temporary = fileManager.temporaryDirectory.appendingPathComponent(AppConfig.engineDirectoryName, isDirectory: true)

        try fileManager.createDirectory(at: working, withIntermediateDirectories: true)
        try fileManager.createDirectory(at: temporary, withIntermediateDirectories: true)

        return EngineDirectories(base: base, working: working, temporary: temporary)
    }
}
#endif
