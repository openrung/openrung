package broker

import (
	"context"
	"time"

	"github.com/openrung/openrung/brokerapi"
)

// In-memory AccessStore. It mirrors the Postgres semantics exactly — fixed
// enrollment windows that reset when they age out, charging only a new
// identity, nothing persisted on refusal — so the conformance test in
// users_test.go can run the same scenarios against both.

// enrollmentCounter is one source's count in one fixed window.
type enrollmentCounter struct {
	windowStart time.Time
	window      time.Duration
	count       int
}

func (s *Store) AuthenticateUser(_ context.Context, canonicalPublicKey, sourceKey string, windows []EnrollmentWindow, now time.Time) (User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.userIDsByKey[canonicalPublicKey]; ok {
		user := s.users[id]
		user.LastAuthAt = now
		s.users[id] = user
		return user, false, nil
	}

	// Evaluate every window before charging any, so a refusal leaves the
	// budget untouched — like the Postgres transaction rolling back.
	counters := s.enrollment[sourceKey]
	if counters == nil {
		counters = make(map[string]enrollmentCounter)
	}
	next := make(map[string]enrollmentCounter, len(windows))
	var retryAfter time.Duration
	for _, w := range windows {
		counter, ok := counters[w.Name]
		if !ok || !counter.windowStart.Add(counter.window).After(now) {
			counter = enrollmentCounter{windowStart: now, window: w.Window, count: 0}
		}
		counter.window = w.Window
		counter.count++
		if counter.count > w.Limit {
			wait := counter.windowStart.Add(counter.window).Sub(now)
			if retryAfter == 0 || wait < retryAfter {
				retryAfter = wait
			}
		}
		next[w.Name] = counter
	}
	if retryAfter > 0 {
		return User{}, false, &EnrollmentLimitedError{RetryAfter: retryAfter}
	}
	for name, counter := range next {
		counters[name] = counter
	}
	if len(counters) > 0 {
		s.enrollment[sourceKey] = counters
	}

	id, err := newUserID()
	if err != nil {
		return User{}, false, err
	}
	user := User{
		ID:         id,
		PublicKey:  canonicalPublicKey,
		Status:     brokerapi.UserStatusEnabled,
		CreatedAt:  now,
		LastAuthAt: now,
	}
	s.users[id] = user
	s.userIDsByKey[canonicalPublicKey] = id
	return user, true, nil
}

func (s *Store) UserByID(_ context.Context, id string) (User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	user, ok := s.users[id]
	if !ok {
		return User{}, ErrUserNotFound
	}
	return user, nil
}

func (s *Store) SetUserStatus(_ context.Context, id, status, reason string, now time.Time) (User, []string, error) {
	if !validUserStatus(status) {
		return User{}, nil, ErrInvalidUserStatus
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[id]
	if !ok {
		return User{}, nil, ErrUserNotFound
	}
	var changed []string
	user.Status = status
	if status == brokerapi.UserStatusDisabled {
		user.DisabledAt = now
		user.DisabledReason = reason
		changed = s.revokeUserLeasesLocked(id, now)
	} else {
		user.DisabledAt = time.Time{}
		user.DisabledReason = ""
	}
	s.users[id] = user
	return user, changed, nil
}

// pruneEnrollmentLocked drops counters whose window has ended; they would
// reset on their next use anyway, so this only bounds memory.
func (s *Store) pruneEnrollmentLocked(now time.Time) {
	for source, counters := range s.enrollment {
		for name, counter := range counters {
			if !counter.windowStart.Add(counter.window).After(now) {
				delete(counters, name)
			}
		}
		if len(counters) == 0 {
			delete(s.enrollment, source)
		}
	}
}
