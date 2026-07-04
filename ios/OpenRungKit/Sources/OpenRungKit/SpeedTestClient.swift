import Foundation

public struct SpeedTestResult: Sendable, Equatable {
    public let bytesDownloaded: Int64
    public let durationMs: Int64
    public let timeToFirstByteMs: Int64
    public let downloadMbps: Double

    public init(bytesDownloaded: Int64, durationMs: Int64, timeToFirstByteMs: Int64, downloadMbps: Double) {
        self.bytesDownloaded = bytesDownloaded
        self.durationMs = durationMs
        self.timeToFirstByteMs = timeToFirstByteMs
        self.downloadMbps = downloadMbps
    }
}

public enum SpeedTestError: Error, Equatable {
    case invalidBrokerURL
    case httpStatus(Int)
    case emptyResponse
}

/// Downloads a warmup payload followed by a measurement payload from the broker's
/// `/api/v1/speed-test` endpoint and reports throughput. Port of Android `SpeedTestClient`.
public struct SpeedTestClient: Sendable {
    public static let defaultWarmupBytes = 1_000_000
    public static let defaultMeasurementBytes = 10_000_000

    private let endpoint: URL
    private let warmupBytes: Int
    private let measurementBytes: Int
    private let session: URLSession

    public init(
        brokerURL: URL,
        warmupBytes: Int = SpeedTestClient.defaultWarmupBytes,
        measurementBytes: Int = SpeedTestClient.defaultMeasurementBytes,
        session: URLSession = .shared
    ) throws {
        self.endpoint = try SpeedTestClient.speedTestURL(brokerURL: brokerURL)
        self.warmupBytes = warmupBytes
        self.measurementBytes = measurementBytes
        self.session = session
    }

    public func run() async throws -> SpeedTestResult {
        _ = try await download(bytes: warmupBytes)
        return try await download(bytes: measurementBytes)
    }

    private func download(bytes: Int) async throws -> SpeedTestResult {
        guard var components = URLComponents(url: endpoint, resolvingAgainstBaseURL: false) else {
            throw SpeedTestError.invalidBrokerURL
        }
        var items = components.queryItems ?? []
        items.append(URLQueryItem(name: "bytes", value: String(bytes)))
        items.append(URLQueryItem(name: "cacheBust", value: String(DispatchTime.now().uptimeNanoseconds)))
        components.queryItems = items
        guard let url = components.url else {
            throw SpeedTestError.invalidBrokerURL
        }

        var request = URLRequest(url: url)
        request.httpMethod = "GET"
        request.timeoutInterval = 60
        request.cachePolicy = .reloadIgnoringLocalAndRemoteCacheData
        request.setValue("identity", forHTTPHeaderField: "Accept-Encoding")

        let collector = SpeedTestMetricsCollector()
        let startedNs = DispatchTime.now().uptimeNanoseconds
        let (data, response) = try await session.data(for: request, delegate: collector)
        let finishedNs = DispatchTime.now().uptimeNanoseconds

        guard let http = response as? HTTPURLResponse else {
            throw SpeedTestError.emptyResponse
        }
        guard (200..<300).contains(http.statusCode) else {
            throw SpeedTestError.httpStatus(http.statusCode)
        }
        let downloaded = Int64(data.count)
        guard downloaded > 0 else {
            throw SpeedTestError.emptyResponse
        }

        let durationNs = max(finishedNs - startedNs, 1)
        let timeToFirstByteNs: UInt64
        if let ttfb = collector.timeToFirstByte {
            timeToFirstByteNs = UInt64(max(ttfb, 0) * 1_000_000_000)
        } else {
            timeToFirstByteNs = durationNs
        }

        return SpeedTestResult(
            bytesDownloaded: downloaded,
            durationMs: Int64(durationNs / 1_000_000),
            timeToFirstByteMs: Int64(timeToFirstByteNs / 1_000_000),
            downloadMbps: SpeedTestClient.calculateMbps(bytes: downloaded, durationNs: durationNs)
        )
    }

    public static func speedTestURL(brokerURL: URL) throws -> URL {
        try BrokerEndpoint.build(base: brokerURL, appending: "api/v1/speed-test")
    }

    public static func calculateMbps(bytes: Int64, durationNs: UInt64) -> Double {
        guard bytes >= 0, durationNs > 0 else { return 0 }
        return Double(bytes) * 8.0 * 1_000.0 / Double(durationNs)
    }
}

/// Captures time-to-first-byte from URLSession task metrics without buffering byte-by-byte.
final class SpeedTestMetricsCollector: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    private(set) var timeToFirstByte: TimeInterval?

    func urlSession(_ session: URLSession, task: URLSessionTask, didFinishCollecting metrics: URLSessionTaskMetrics) {
        guard let transaction = metrics.transactionMetrics.last else { return }
        let start = transaction.requestStartDate ?? transaction.fetchStartDate
        if let start, let responseStart = transaction.responseStartDate {
            timeToFirstByte = responseStart.timeIntervalSince(start)
        }
    }
}
