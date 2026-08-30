package clienttelemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/openrung/openrung/brokerapi"
)

// This file is the shared on-disk telemetry outbox (ADR-003 A3): the queue
// policy D4 bound into both mobile platforms — enqueue cap with oldest-first
// eviction, identity-homogeneous upload batches under the broker's
// per-application flow budget, heartbeat piggybacking that can never let
// backlog delay the heartbeat — together with the hardening mobile #97 added:
// validated legacy migration, fsync durability for imports whose only other
// copy the caller deletes, torn-tail repair, an advisory cross-process lock,
// and a load-failure latch so a transient read error never reads as an empty
// queue. It moved here from the mobile binding
// (openrung-mobile-app/android/punchbridge/telemetry_binding.go) so the
// engine's Manager and the gomobile binding drive ONE implementation; the
// binding keeps only its gomobile adapter (JSON-string boundary, platform
// posting headers, the cancelable single-use upload wrapper).

const (
	// OutboxMaxQueued caps the persisted outbox, oldest dropped first — the
	// value both platform outboxes enforced (and the Manager's in-memory cap).
	OutboxMaxQueued = maxQueuedEvents
	// outboxBatchSize is one upload request's event budget, brokerapi's own
	// maximum (and the Manager's in-memory batch size).
	outboxBatchSize = uploadBatchSize
	// outboxCompactThreshold bounds the append-only file: past twice the
	// in-memory cap it is rewritten from the cache, so append cost stays O(1)
	// amortized per event.
	outboxCompactThreshold = 2 * OutboxMaxQueued

	// applicationConnectionEvent rows carry only the application identity;
	// the broker keeps an hourly per-application count and discards
	// everything else, so their attributes are scrubbed on every load and
	// enqueue (which also scrubs a pre-upgrade backlog before either upload
	// path can put it on the wire).
	applicationConnectionEvent = "application_connection"
	applicationCountKey        = "connection_count"
	// maxReportedFlows is the broker's represented-flow budget per
	// application and upload request (ApplicationConnectionAggregator's
	// MAX_REPORTED_FLOWS).
	maxReportedFlows = int64(100_000)
)

var (
	// ErrOutboxUnavailable: the queue is unreadable this operation — a
	// transient file error, or another process owns the outbox. Nothing was
	// sent or drained; the next operation retries.
	ErrOutboxUnavailable = errors.New("telemetry outbox unavailable")
	// ErrInvalidTelemetryEvent: the event lacks the identity fields every
	// upload derives its headers from.
	ErrInvalidTelemetryEvent = errors.New("invalid telemetry event")
	// ErrOutboxClosed: the outbox was closed; queued events belong to the
	// next open.
	ErrOutboxClosed = errors.New("telemetry outbox closed")
)

// SendTelemetryBatch posts one identity-homogeneous batch to the broker. The
// engine's implementation goes through HTTPClient; the mobile binding's goes
// through brokerapi's binding bridge with its platform posting headers.
type SendTelemetryBatch func(ctx context.Context, brokerURL string, events []Event) error

// Outbox is the on-disk telemetry outbox: an append-only NDJSON file (one
// Event per line) in a host-supplied directory, plus the upload policy over
// it. All methods are safe for concurrent use. Network sends hold no lock, so
// enqueues proceed during an upload; sent events are removed only after their
// request succeeded, atomically with the send outcome.
type Outbox struct {
	mu sync.Mutex

	path string
	send SendTelemetryBatch

	// lockFile holds the advisory cross-process lock on the outbox for this
	// outbox's lifetime (see loadLocked); nil until the first successful load.
	lockFile *os.File

	loaded    bool
	events    []Event
	fileLines int
	closed    bool
}

// NewOutbox opens (or creates) the outbox file fileName inside directory.
// fileName must be a bare name — the file lives exactly where the host said.
func NewOutbox(directory, fileName string, send SendTelemetryBatch) (*Outbox, error) {
	if directory == "" || fileName == "" ||
		fileName != filepath.Base(fileName) || fileName == "." || fileName == ".." {
		return nil, errors.New("telemetry outbox needs a directory and a bare file name")
	}
	if send == nil {
		return nil, errors.New("telemetry outbox needs a send function")
	}
	return &Outbox{
		path: filepath.Join(directory, fileName),
		send: send,
	}, nil
}

// Enqueue appends one event. An event without the identity fields is dropped
// and reported false; the outbox itself stays usable — telemetry must never
// take down the reporting path.
func (o *Outbox) Enqueue(event Event) bool {
	event, ok := validateOutboxEvent(event)
	if !ok {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || !o.loadLocked() {
		return false
	}
	o.appendLocked([]Event{event})
	return true
}

// EnqueueBatch appends every valid event, oldest first. It exists for the
// platforms' pre-file legacy stores (Android's SharedPreferences blob), whose
// one-time import must land through the same cap and sanitization as live
// events — and whose only durable copy the caller deletes on this method's
// word. The return value is therefore a three-way answer: the accepted count
// once the events are DURABLY persisted to the outbox file, 0 for a batch
// that holds nothing importable (the import is complete; the caller may clear
// its store), and -1 when the events could not be durably written (closed
// outbox, unwritable directory) — the caller must keep its copy and retry on
// the next open.
func (o *Outbox) EnqueueBatch(events []Event) int {
	valid := make([]Event, 0, len(events))
	for _, event := range events {
		if event, ok := validateOutboxEvent(event); ok {
			valid = append(valid, event)
		}
	}
	if len(valid) == 0 {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || !o.loadLocked() {
		return -1
	}
	if !o.appendLocked(valid) {
		// The events stay in the in-memory queue (a later flush may still
		// upload them), but nothing durable landed: the caller keeps its copy.
		return -1
	}
	if syncOutboxFile(o.path) != nil {
		// The append landed only in the page cache; a crash could still lose
		// it, so the caller must keep its copy. The events remain queued.
		return -1
	}
	return len(valid)
}

// ApplySessionAttributes merges attributes (new values winning) into every
// queued event of sessionID — the geo back-patch, so events recorded before
// the public-IP lookup resolved still carry it. application_connection rows
// are never patched; their attributes stay empty.
func (o *Outbox) ApplySessionAttributes(sessionID string, attributes map[string]string) bool {
	if sessionID == "" || len(attributes) == 0 {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || !o.loadLocked() {
		return false
	}
	changed := false
	for i := range o.events {
		event := &o.events[i]
		if event.SessionID != sessionID || event.Event == applicationConnectionEvent {
			continue
		}
		if event.Attributes == nil {
			event.Attributes = make(map[string]string, len(attributes))
		}
		for key, value := range attributes {
			if event.Attributes[key] != value {
				event.Attributes[key] = value
				changed = true
			}
		}
	}
	if changed {
		o.rewriteLocked()
	}
	return changed
}

// PendingCount reports the queued event count (loading the file on first use).
func (o *Outbox) PendingCount() int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || !o.loadLocked() {
		return 0
	}
	return len(o.events)
}

// FlushNextBatch uploads at most one batch — the queue head's identity-
// homogeneous prefix under the per-application flow budget — and removes it
// from the outbox on success. An empty queue succeeds with sent 0. Callers
// loop until pending reaches 0, keeping their own cancellation between
// requests.
func (o *Outbox) FlushNextBatch(ctx context.Context, brokerURL string) (sent, pending int, err error) {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return 0, 0, ErrOutboxClosed
	}
	if !o.loadLocked() {
		// The queue is unreadable this attempt: fail the flush rather than
		// report an empty, drained queue.
		o.mu.Unlock()
		return 0, 0, ErrOutboxUnavailable
	}
	batch := outboxUploadBatch(o.events, outboxBatchSize)
	pending = len(o.events)
	o.mu.Unlock()

	if len(batch) == 0 {
		return 0, pending, nil
	}
	if err := o.post(ctx, brokerURL, batch); err != nil {
		return 0, pending, err
	}
	return len(batch), o.removeSent(batch), nil
}

// SendHeartbeat uploads heartbeat, letting the queue head's identity-
// homogeneous prefix piggyback only when it matches the heartbeat's own
// client/session pair — a historical backlog (or a failure uploading it) must
// never suppress heartbeat cadence, so any other head sends the heartbeat
// alone. Piggybacked events are removed on success; the heartbeat itself is
// never persisted (callers rebuild it each cadence). Callers drain what
// remains with FlushNextBatch uploads afterwards.
func (o *Outbox) SendHeartbeat(ctx context.Context, brokerURL string, heartbeat Event) (sent, pending int, err error) {
	heartbeat, ok := validateOutboxEvent(heartbeat)
	if !ok {
		return 0, o.PendingCount(), ErrInvalidTelemetryEvent
	}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return 0, 0, ErrOutboxClosed
	}
	// A failed load must not block the cadence: the heartbeat goes alone and
	// the queue is retried by the next operation.
	_ = o.loadLocked()
	var piggybacked []Event
	if len(o.events) > 0 &&
		o.events[0].ClientID == heartbeat.ClientID &&
		o.events[0].SessionID == heartbeat.SessionID {
		piggybacked = outboxUploadBatch(o.events, outboxBatchSize-1)
	}
	pending = len(o.events)
	o.mu.Unlock()

	if err := o.post(ctx, brokerURL, append(piggybacked[:len(piggybacked):len(piggybacked)], heartbeat)); err != nil {
		return 0, pending, err
	}
	if len(piggybacked) > 0 {
		pending = o.removeSent(piggybacked)
	}
	return len(piggybacked) + 1, pending, nil
}

// Close cancels further use and releases the cross-process file lock. It
// deliberately does not touch the outbox file: queued events belong to the
// next open. In-flight sends may still complete; their commit refuses to
// rewrite a file this outbox no longer owns (see removeSent).
func (o *Outbox) Close() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.closed = true
	if o.lockFile != nil {
		unlockOutboxFile(o.lockFile)
		_ = o.lockFile.Close()
		o.lockFile = nil
	}
	o.mu.Unlock()
}

// post validates the broker URL shape before anything reaches the wire, then
// hands the batch to the host's send function.
func (o *Outbox) post(ctx context.Context, brokerURL string, events []Event) error {
	if _, err := brokerapi.TelemetryURL(brokerURL); err != nil {
		return err
	}
	return o.send(ctx, brokerURL, events)
}

func (o *Outbox) removeSent(sent []Event) int {
	sentIDs := make(map[string]struct{}, len(sent))
	for _, event := range sent {
		sentIDs[event.EventID] = struct{}{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		// The send runs outside the mutex, so it can complete while racing
		// Close — and Close released the cross-process lock, meaning another
		// process may own the file by now. A rewrite from this cache could
		// overwrite that owner's queue, so refuse to mutate: the accepted
		// batch stays queued, and re-delivering it later is the safe side.
		return len(o.events)
	}
	kept := o.events[:0]
	removed := false
	for _, event := range o.events {
		if _, ok := sentIDs[event.EventID]; ok {
			removed = true
			continue
		}
		kept = append(kept, event)
	}
	o.events = kept
	if removed {
		o.rewriteLocked()
	}
	return len(o.events)
}

// appendLocked queues events and lands them on disk, reporting whether the
// events are now durably persisted (directly appended, or via the rewrite a
// failed append and the compaction both fall back to).
func (o *Outbox) appendLocked(events []Event) bool {
	o.loadLocked()
	o.events = append(o.events, events...)
	if len(o.events) > OutboxMaxQueued {
		o.events = append([]Event(nil), o.events[len(o.events)-OutboxMaxQueued:]...)
	}

	var lines []byte
	appendable := true
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			appendable = false
			break
		}
		lines = append(lines, line...)
		lines = append(lines, '\n')
	}
	if appendable {
		appendable = appendOutboxFile(o.path, lines) == nil
		o.fileLines += len(events)
	}
	// The file may hold more lines than the cap between compactions; a failed
	// append falls back to the same durable rewrite the compaction uses.
	if !appendable || o.fileLines > outboxCompactThreshold {
		return o.rewriteLocked()
	}
	return true
}

// loadLocked populates the cache from the outbox file on first use: NDJSON
// lines (blank, torn, or otherwise undecodable lines skipped) or the
// pre-NDJSON single-JSON-array format, folded in and rewritten as NDJSON — the
// one-time format migration. Loading also re-applies the cap and the
// application_connection scrub to any pre-upgrade backlog, rewriting the file
// whenever what it holds is not exactly what was loaded. It reports false when
// the outbox is unavailable this operation — the file is unreadable, or
// another process owns it — in which case nothing is cached and the next
// operation retries.
func (o *Outbox) loadLocked() bool {
	if o.loaded {
		return true
	}
	// One process owns the outbox file for the outbox's lifetime. The lock is
	// advisory, taken on the first load, and released by Close or process
	// death. A second process of the same app (the iOS app beside its
	// PacketTunnel extension shares the App Group container) degrades to an
	// unavailable outbox instead of overwriting the owner's queue with its own
	// stale cache — the coordination the platform outboxes' NSFileCoordinator
	// previously provided.
	if o.lockFile == nil {
		lock, err := os.OpenFile(o.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return false
		}
		if err := lockOutboxFile(lock); err != nil {
			_ = lock.Close()
			return false
		}
		o.lockFile = lock
	}
	raw, err := os.ReadFile(o.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// A transient read failure must not read as an empty queue: a later
		// rewrite would replace the intact backlog on disk with the empty
		// cache. Stay unloaded so the next operation retries.
		return false
	}
	o.loaded = true
	if err != nil || len(raw) == 0 {
		o.events = nil
		o.fileLines = 0
		return true
	}

	var parsed []Event
	dirty := false
	sourceLines := 0
	if raw[0] == '[' {
		// The pre-NDJSON array format (iOS before 0.3.5), validated element by
		// element exactly like live enqueues — a row without the identity
		// fields can never anchor (and permanently poison) an upload batch,
		// and one undecodable element cannot discard the decodable remainder.
		// Always rewritten as NDJSON below: the one-time format migration.
		dirty = true
		var rawEvents []json.RawMessage
		if json.Unmarshal(raw, &rawEvents) == nil {
			for _, message := range rawEvents {
				if event, ok := decodeOutboxEvent(string(message)); ok {
					parsed = append(parsed, event)
				}
			}
		}
	} else {
		if raw[len(raw)-1] != '\n' {
			// An unterminated tail line that still decodes would fuse with the
			// next append into one undecodable line, losing both events; the
			// rewrite below re-terminates it (the torn-tail repair the platform
			// outboxes performed before every append).
			dirty = true
		}
		lines := strings.Split(string(raw), "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			sourceLines++
			if event, ok := decodeOutboxEvent(line); ok {
				if outboxLineNeedsScrub(line, event) {
					// The decode scrubbed data the stored line still carries;
					// persist the scrub instead of leaving it on disk.
					dirty = true
				}
				parsed = append(parsed, event)
			}
		}
	}

	events := make([]Event, 0, len(parsed))
	for _, event := range parsed {
		events = append(events, sanitizeOutboxEvent(event))
	}
	if len(events) > OutboxMaxQueued {
		events = events[len(events)-OutboxMaxQueued:]
	}
	o.events = events
	o.fileLines = len(events)
	if dirty || sourceLines != len(events) {
		if !o.rewriteLocked() {
			// The file still holds what could not be repaired or migrated:
			// appending to it would corrupt it (an NDJSON line after a legacy
			// array's closing bracket, an event fused onto an unterminated
			// tail), and the scrub would exist only in memory. Stay unloaded
			// so mutating operations degrade and the next one retries.
			o.loaded = false
			o.events = nil
			o.fileLines = 0
			return false
		}
	}
	return true
}

// rewriteLocked lands the cache as one durable file — temp write, fsync,
// atomic rename, so a kill mid-rewrite leaves the previous complete file in
// place — and reports whether the file now holds the cache.
func (o *Outbox) rewriteLocked() bool {
	var lines []byte
	for _, event := range o.events {
		line, err := json.Marshal(event)
		if err != nil {
			continue
		}
		lines = append(lines, line...)
		lines = append(lines, '\n')
	}
	temp := o.path + ".tmp"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	_, writeErr := file.Write(lines)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(temp)
		return false
	}
	if err := os.Rename(temp, o.path); err != nil {
		// No in-place fallback: overwriting the live file directly is not
		// atomic, and a false answer already means "the file does not hold the
		// cache" to every caller.
		_ = os.Remove(temp)
		return false
	}
	_ = syncOutboxDir(filepath.Dir(o.path))
	o.fileLines = len(o.events)
	return true
}

func appendOutboxFile(path string, lines []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(lines)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

// decodeOutboxEvent accepts one stored Event line, ignoring unknown keys
// (which is what scrubs the removed destination_* fields from a pre-upgrade
// backlog) and rejecting events without the identity fields every upload
// derives its headers from.
func decodeOutboxEvent(eventJSON string) (Event, bool) {
	var event Event
	if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
		return Event{}, false
	}
	return validateOutboxEvent(event)
}

// validateOutboxEvent rejects events without the identity fields every upload
// derives its headers from, sanitizing what it accepts.
func validateOutboxEvent(event Event) (Event, bool) {
	if event.EventID == "" || event.Event == "" || event.ClientID == "" || event.SessionID == "" {
		return Event{}, false
	}
	return sanitizeOutboxEvent(event), true
}

// sanitizeOutboxEvent removes client metadata the broker never retains from
// application-connection records.
func sanitizeOutboxEvent(event Event) Event {
	if event.Event == applicationConnectionEvent && len(event.Attributes) > 0 {
		event.Attributes = nil
	}
	return event
}

// outboxUploadBatch selects one upload request from the first event's
// client/session identity prefix while honoring the broker's represented-flow
// budget per application. The identity boundary is strict: brokerapi rejects
// mixed pairs, and later sessions cannot leapfrog the head session. Within
// that prefix, events that exceed an application's remaining budget are
// deferred along with later events for that application; unrelated events may
// still fill the request.
func outboxUploadBatch(events []Event, limit int) []Event {
	if limit <= 0 || len(events) == 0 {
		return nil
	}
	first := events[0]
	representedByApplication := make(map[string]int64)
	deferredApplications := make(map[string]struct{})
	var batch []Event
	for _, event := range events {
		if event.ClientID != first.ClientID || event.SessionID != first.SessionID {
			break
		}
		if len(batch) >= limit {
			break
		}
		if event.Event != applicationConnectionEvent {
			batch = append(batch, cloneOutboxEvent(event))
			continue
		}
		application := event.Application
		if _, deferred := deferredApplications[application]; deferred {
			continue
		}
		count := applicationConnectionCount(event)
		used := representedByApplication[application]
		if count > maxReportedFlows-used {
			deferredApplications[application] = struct{}{}
			continue
		}
		representedByApplication[application] = used + count
		batch = append(batch, cloneOutboxEvent(event))
	}
	return batch
}

// cloneOutboxEvent gives a batch entry its own attribute and measurement
// maps: the send marshals the batch outside the outbox lock, and sharing map
// headers with the live queue would race the geo back-patch's under-lock
// writes — a fatal concurrent map access.
func cloneOutboxEvent(event Event) Event {
	event.Attributes = maps.Clone(event.Attributes)
	event.Measurements = maps.Clone(event.Measurements)
	return event
}

// outboxLineNeedsScrub reports whether a stored line still carries data the
// decode scrubbed from the loaded event — the removed destination_* and
// protocol keys any pre-upgrade event may hold (their JSON null spellings
// included), or attributes on an application_connection row — so the load can
// persist the scrub instead of leaving the data on disk indefinitely.
func outboxLineNeedsScrub(line string, event Event) bool {
	var probe struct {
		Attributes      map[string]json.RawMessage `json:"attributes"`
		DestinationIP   json.RawMessage            `json:"destination_ip"`
		DestinationPort json.RawMessage            `json:"destination_port"`
		Protocol        json.RawMessage            `json:"protocol"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil {
		return false
	}
	if probe.DestinationIP != nil || probe.DestinationPort != nil || probe.Protocol != nil {
		return true
	}
	return event.Event == applicationConnectionEvent && len(probe.Attributes) > 0
}

// syncOutboxFile flushes the outbox file and its directory entry to stable
// storage — the legacy import deletes its only other copy on this promise, so
// "accepted" must survive a power loss.
func syncOutboxFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	return syncOutboxDir(filepath.Dir(path))
}

// applicationConnectionCount mirrors the broker's compatibility behavior for
// missing or malformed typed counts.
func applicationConnectionCount(event Event) int64 {
	count, ok := event.Measurements[applicationCountKey]
	if !ok {
		return 1
	}
	if count < 1 || count > maxReportedFlows {
		return 1
	}
	return count
}
