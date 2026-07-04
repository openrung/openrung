package broker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxTelemetryBodyBytes  = 512 << 10
	maxTelemetryEvents     = 200
	maxSpeedTestBytes      = 25_000_000
	telemetryRetention     = 7 * 24 * time.Hour
	telemetryFlushInterval = 5 * time.Second
	telemetrySyncInterval  = 30 * time.Second
)

func speedTestHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requested, err := strconv.Atoi(r.URL.Query().Get("bytes"))
		if err != nil || requested < 1 || requested > maxSpeedTestBytes {
			writeError(w, http.StatusBadRequest, "bytes must be between 1 and 25000000")
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(requested))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyN(w, speedTestReader{}, int64(requested))
	}
}

type speedTestReader struct{}

func (speedTestReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = byte((index*31 + 17) % 251)
	}
	return len(buffer), nil
}

type TelemetryEvent struct {
	SchemaVersion   int               `json:"schema_version"`
	EventID         string            `json:"event_id"`
	Event           string            `json:"event"`
	OccurredAt      time.Time         `json:"occurred_at"`
	ClientID        string            `json:"client_id"`
	SessionID       string            `json:"session_id"`
	RelayID         string            `json:"relay_id,omitempty"`
	Application     string            `json:"application_package,omitempty"`
	ApplicationID   int               `json:"application_uid,omitempty"`
	DestinationIP   string            `json:"destination_ip,omitempty"`
	DestinationPort int               `json:"destination_port,omitempty"`
	Protocol        string            `json:"protocol,omitempty"`
	Attributes      map[string]string `json:"attributes,omitempty"`
	Measurements    map[string]int64  `json:"measurements,omitempty"`
}

type telemetryBatch struct {
	Events []TelemetryEvent `json:"events"`
}

type TelemetryRecord struct {
	ReceivedAt time.Time      `json:"received_at"`
	SourceIP   string         `json:"source_ip"`
	Event      TelemetryEvent `json:"event"`
}

type TelemetrySink interface {
	WriteTelemetry(context.Context, []TelemetryRecord) error
}

type TelemetryReader interface {
	TelemetryRecords(time.Time) []TelemetryRecord
}

type JSONLTelemetrySink struct {
	mu        sync.RWMutex
	file      *os.File
	writer    *bufio.Writer
	records   []TelemetryRecord
	now       func() time.Time
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
	writeErr  error
	closed    bool
}

func NewJSONLTelemetrySink(path string) (*JSONLTelemetrySink, error) {
	return newJSONLTelemetrySink(path, time.Now)
}

func newJSONLTelemetrySink(path string, now func() time.Time) (*JSONLTelemetrySink, error) {
	return newJSONLTelemetrySinkWithIntervals(path, now, telemetryFlushInterval, telemetrySyncInterval)
}

func newJSONLTelemetrySinkWithIntervals(path string, now func() time.Time, flushInterval, syncInterval time.Duration) (*JSONLTelemetrySink, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("telemetry file path is required")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create telemetry directory: %w", err)
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open telemetry file: %w", err)
	}
	sink := &JSONLTelemetrySink{
		file: file, now: now, stop: make(chan struct{}), done: make(chan struct{}),
	}
	if err := sink.loadRecent(); err != nil {
		_ = file.Close()
		return nil, err
	}
	sink.writer = bufio.NewWriter(file)
	go sink.persistenceLoop(flushInterval, syncInterval)
	return sink, nil
}

func (s *JSONLTelemetrySink) loadRecent() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek telemetry file: %w", err)
	}
	cutoff := s.now().UTC().Add(-telemetryRetention)
	scanner := bufio.NewScanner(s.file)
	scanner.Buffer(make([]byte, 64<<10), maxTelemetryBodyBytes*2)
	line := 0
	for scanner.Scan() {
		line++
		var record TelemetryRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("decode telemetry record on line %d: %w", line, err)
		}
		if !record.Event.OccurredAt.Before(cutoff) {
			s.records = append(s.records, record)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read telemetry file: %w", err)
	}
	_, err := s.file.Seek(0, io.SeekEnd)
	return err
}

func (s *JSONLTelemetrySink) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.mu.Lock()
		defer s.mu.Unlock()
		s.closed = true
		if err := s.flushLocked(true); err != nil {
			s.closeErr = err
		}
		if err := s.file.Close(); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

func (s *JSONLTelemetrySink) WriteTelemetry(_ context.Context, records []TelemetryRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("telemetry storage is closed")
	}
	if s.writeErr != nil {
		return s.writeErr
	}

	encoder := json.NewEncoder(s.writer)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return fmt.Errorf("encode telemetry record: %w", err)
		}
	}
	s.records = append(s.records, records...)
	s.pruneLocked(s.now().UTC().Add(-telemetryRetention))
	return nil
}

func (s *JSONLTelemetrySink) persistenceLoop(flushInterval, syncInterval time.Duration) {
	flushTicker := time.NewTicker(flushInterval)
	syncTicker := time.NewTicker(syncInterval)
	defer func() {
		flushTicker.Stop()
		syncTicker.Stop()
		close(s.done)
	}()
	for {
		select {
		case <-flushTicker.C:
			s.flush(false)
		case <-syncTicker.C:
			s.flush(true)
		case <-s.stop:
			return
		}
	}
}

func (s *JSONLTelemetrySink) flush(syncDisk bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.flushLocked(syncDisk)
}

func (s *JSONLTelemetrySink) flushLocked(syncDisk bool) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	if err := s.writer.Flush(); err != nil {
		s.writeErr = fmt.Errorf("flush telemetry records: %w", err)
		return s.writeErr
	}
	if syncDisk {
		if err := s.file.Sync(); err != nil {
			s.writeErr = fmt.Errorf("sync telemetry records: %w", err)
			return s.writeErr
		}
	}
	return nil
}

func (s *JSONLTelemetrySink) TelemetryRecords(since time.Time) []TelemetryRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	records := make([]TelemetryRecord, 0, len(s.records))
	for _, record := range s.records {
		if !record.Event.OccurredAt.Before(since) {
			records = append(records, record)
		}
	}
	return records
}

func (s *JSONLTelemetrySink) pruneLocked(cutoff time.Time) {
	kept := s.records[:0]
	for _, record := range s.records {
		if !record.Event.OccurredAt.Before(cutoff) {
			kept = append(kept, record)
		}
	}
	s.records = kept
}

func telemetryHandler(sink TelemetrySink, relayMetrics RelayStore, clientIP *clientIPResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if sink == nil {
			writeError(w, http.StatusServiceUnavailable, "telemetry is not configured")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxTelemetryBodyBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var batch telemetryBatch
		if err := decoder.Decode(&batch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid telemetry JSON")
			return
		}
		if len(batch.Events) == 0 || len(batch.Events) > maxTelemetryEvents {
			writeError(w, http.StatusBadRequest, "events must contain between 1 and 200 items")
			return
		}

		now := time.Now().UTC()
		sourceIP := clientIP.clientIP(r)
		records := make([]TelemetryRecord, 0, len(batch.Events))
		for _, event := range batch.Events {
			if err := validateTelemetryEvent(event); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			records = append(records, TelemetryRecord{ReceivedAt: now, SourceIP: sourceIP, Event: event})
		}
		if err := sink.WriteTelemetry(r.Context(), records); err != nil {
			slog.Error("could not store telemetry", "records", len(records), "error", err)
			writeError(w, http.StatusInternalServerError, "could not store telemetry")
			return
		}
		if relayMetrics != nil {
			if err := relayMetrics.RecordRelayTelemetry(r.Context(), records, now); err != nil {
				slog.Error("could not store relay metrics", "records", len(records), "error", err)
				writeError(w, http.StatusServiceUnavailable, "could not store relay metrics")
				return
			}
		}
		writeJSON(w, http.StatusAccepted, map[string]int{"accepted": len(records)})
	}
}

func validateTelemetryEvent(event TelemetryEvent) error {
	switch {
	case event.SchemaVersion != 1:
		return errors.New("schema_version must be 1")
	case strings.TrimSpace(event.EventID) == "" || len(event.EventID) > 128:
		return errors.New("event_id is required and must be at most 128 characters")
	case strings.TrimSpace(event.Event) == "" || len(event.Event) > 64:
		return errors.New("event is required and must be at most 64 characters")
	case strings.TrimSpace(event.ClientID) == "" || len(event.ClientID) > 128:
		return errors.New("client_id is required and must be at most 128 characters")
	case strings.TrimSpace(event.SessionID) == "" || len(event.SessionID) > 128:
		return errors.New("session_id is required and must be at most 128 characters")
	case event.OccurredAt.IsZero():
		return errors.New("occurred_at is required")
	case len(event.Attributes) > 32 || len(event.Measurements) > 32:
		return errors.New("telemetry maps may contain at most 32 entries")
	}
	return nil
}

func recordClientSeen(r *http.Request, sink TelemetrySink, clientIP *clientIPResolver) {
	clientID := strings.TrimSpace(r.Header.Get("X-OpenRung-Client-ID"))
	sessionID := strings.TrimSpace(r.Header.Get("X-OpenRung-Session-ID"))
	if sink == nil || clientID == "" || sessionID == "" || len(clientID) > 128 || len(sessionID) > 128 {
		return
	}
	now := time.Now().UTC()
	event := TelemetryEvent{
		SchemaVersion: 1,
		EventID:       "client-seen-" + sessionID,
		Event:         "client_seen",
		OccurredAt:    now,
		ClientID:      clientID,
		SessionID:     sessionID,
		Attributes: map[string]string{
			"app_version": r.Header.Get("X-OpenRung-App-Version"),
			"android_api": r.Header.Get("X-OpenRung-Android-API"),
		},
	}
	if err := sink.WriteTelemetry(r.Context(), []TelemetryRecord{{
		ReceivedAt: now,
		SourceIP:   clientIP.clientIP(r),
		Event:      event,
	}}); err != nil {
		slog.Error("could not store client-seen telemetry", "error", err)
	}
}
