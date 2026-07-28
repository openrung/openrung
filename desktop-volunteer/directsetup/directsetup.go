// Package directsetup manages the optional, least-privilege local setup that
// lets OpenRung Volunteer's public listener use TCP 443.
//
// Merely constructing a Manager never requests elevation. Enable and Remove
// are the only methods that may do so, and callers must expose those methods
// only behind explicit user actions.
package directsetup

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
)

const DirectPort = 443

// State is the user-facing state of the local TCP 443 setup.
type State string

const (
	StateReady       State = "ready"
	StateNeedsSetup  State = "needs_setup"
	StateUnavailable State = "unavailable"
)

// Stable reason values let the frontend choose concise, actionable copy while
// Message retains the platform-specific detail.
const (
	ReasonReady                  = "ready"
	ReasonLowPortsUnprivileged   = "low_ports_unprivileged"
	ReasonFirewallRuleMissing    = "firewall_rule_missing"
	ReasonFirewallRuleMismatch   = "firewall_rule_mismatch"
	ReasonCapabilityMissing      = "capability_missing"
	ReasonCapabilityMismatch     = "capability_mismatch"
	ReasonCapabilityIneffective  = "capability_ineffective"
	ReasonInsecureExecutable     = "insecure_executable"
	ReasonRestartRequired        = "restart_required"
	ReasonRemovalRestartRequired = "removal_restart_required"
	ReasonToolUnavailable        = "tool_unavailable"
	ReasonFilesystemUnsupported  = "filesystem_unsupported"
	ReasonReleaseSigningRequired = "release_signing_required"
	ReasonUnsupportedPlatform    = "unsupported_platform"
	ReasonInspectionFailed       = "inspection_failed"
	ReasonAuthorizationDeclined  = "authorization_declined"
	ReasonAuthorizationDenied    = "authorization_denied"
	ReasonSetupFailed            = "setup_failed"
)

// Status is safe to expose directly through Wails.
//
// Ready only describes local OS setup. It deliberately does not claim that a
// router, NAT mapping, host firewall, or cloud security group permits inbound
// Internet traffic. In Automatic mode, the relay's nonce callback remains the
// source of truth for external reachability; Direct-only is an explicit
// operator override and does not use RelayHub for that check.
type Status struct {
	Platform  string `json:"platform"`
	State     State  `json:"state"`
	Reason    string `json:"reason"`
	CanEnable bool   `json:"canEnable"`
	CanRemove bool   `json:"canRemove"`
	Port      int    `json:"port"`
	Message   string `json:"message"`
}

// Controller is the small interface used by volunteerservice. Tests can inject
// a fake without executing a platform command or changing machine state.
type Controller interface {
	Status(context.Context) Status
	Enable(context.Context) (Status, error)
	Remove(context.Context) (Status, error)
}

// Inspection is returned by a platform implementation after read-only checks.
// Configured means that setup created by OpenRung was identified exactly and
// can be safely removed. Unknown or broader privileges must never set it.
type Inspection struct {
	Ready      bool
	Available  bool
	Configured bool
	Reason     string
	Message    string
}

// Platform contains the privileged boundary. Inspect must be read-only.
// Enable and Remove may request authorization, but Manager only calls them from
// the corresponding explicit methods.
type Platform interface {
	Inspect(context.Context) (Inspection, error)
	Enable(context.Context) error
	Remove(context.Context) error
}

// Manager provides idempotence and consistent error/status handling around a
// platform implementation.
type Manager struct {
	platformName string
	platform     Platform
	mu           sync.Mutex
}

// NewManager constructs the native implementation. It performs no command and
// never requests elevation.
func NewManager() *Manager {
	return NewManagerWithPlatform(runtime.GOOS, newPlatform())
}

// NewManagerWithPlatform is primarily intended for focused tests and alternate
// packaging integrations.
func NewManagerWithPlatform(platformName string, platform Platform) *Manager {
	return &Manager{platformName: platformName, platform: platform}
}

func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusLocked(ctx)
}

func (m *Manager) Enable(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	before := m.statusLocked(ctx)
	if before.State == StateReady || (before.State == StateNeedsSetup && !before.CanEnable) {
		return before, nil
	}
	if before.State == StateUnavailable {
		return before, fmt.Errorf("direct TCP 443 setup is unavailable: %s", before.Message)
	}

	if err := m.platform.Enable(ctx); err != nil {
		status := before
		var opErr *OperationError
		if errors.As(err, &opErr) {
			status.Reason = opErr.Reason
			if opErr.Message != "" {
				status.Message = opErr.Message
			}
			if opErr.Unavailable {
				status.State = StateUnavailable
				status.CanEnable = false
				status.CanRemove = false
			}
		}
		return status, fmt.Errorf("enable direct TCP 443: %w", err)
	}

	after := m.statusLocked(ctx)
	// Linux file capabilities take effect on the next exec. That is a
	// successful, configured setup even though the current GUI must restart.
	if after.State != StateReady && after.Reason != ReasonRestartRequired {
		return after, errors.New("direct TCP 443 setup completed but could not be verified")
	}
	return after, nil
}

func (m *Manager) Remove(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	before := m.statusLocked(ctx)
	if !before.CanRemove {
		return before, nil
	}
	if err := m.platform.Remove(ctx); err != nil {
		status := before
		var opErr *OperationError
		if errors.As(err, &opErr) {
			status.Reason = opErr.Reason
			if opErr.Message != "" {
				status.Message = opErr.Message
			}
		}
		return status, fmt.Errorf("remove direct TCP 443 setup: %w", err)
	}
	after := m.statusLocked(ctx)
	if after.CanRemove {
		return after, errors.New("direct TCP 443 setup removal completed but could not be verified")
	}
	return after, nil
}

func (m *Manager) statusLocked(ctx context.Context) Status {
	inspection, err := m.platform.Inspect(ctx)
	if err != nil {
		return Status{
			Platform: m.platformName,
			State:    StateUnavailable,
			Reason:   ReasonInspectionFailed,
			Port:     DirectPort,
			Message:  "Local TCP 443 setup could not be inspected: " + err.Error(),
		}
	}

	status := Status{
		Platform:  m.platformName,
		Reason:    inspection.Reason,
		CanRemove: inspection.Configured,
		Port:      DirectPort,
		Message:   inspection.Message,
	}
	switch {
	case inspection.Ready:
		status.State = StateReady
	case inspection.Available:
		status.State = StateNeedsSetup
		status.CanEnable = true
		// A configured Linux capability can be waiting for a restart. Re-running
		// an elevation flow cannot activate it in the current process.
		if inspection.Reason == ReasonRestartRequired {
			status.CanEnable = false
		}
	default:
		status.State = StateUnavailable
	}
	return status
}

// OperationError classifies a failed authorization/setup operation without
// conflating it with external reachability. Unavailable is reserved for local
// conditions that retrying authorization cannot fix.
type OperationError struct {
	Reason      string
	Message     string
	Unavailable bool
	Err         error
}

func (e *OperationError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Reason
}

func (e *OperationError) Unwrap() error { return e.Err }

// InitialStatus is used before the application's startup hook completes its
// first read-only platform inspection.
func InitialStatus() Status {
	return Status{
		Platform: runtime.GOOS,
		State:    StateUnavailable,
		Reason:   "checking",
		Port:     DirectPort,
		Message:  "Checking local TCP 443 setup…",
	}
}

// HandlePrivilegedCommand handles a narrowly defined helper invocation and
// returns whether the normal GUI startup should be skipped. It is a no-op on
// platforms that do not use the application binary as a privileged helper.
func HandlePrivilegedCommand(args []string) (handled bool, exitCode int) {
	return handlePrivilegedCommand(args)
}
