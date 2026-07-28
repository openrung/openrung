package directsetup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakePlatform struct {
	inspection  Inspection
	inspectErr  error
	enableErr   error
	removeErr   error
	enableCalls int
	removeCalls int
	onEnable    func(*fakePlatform)
	onRemove    func(*fakePlatform)
}

type blockingInspectionPlatform struct {
	receivedDeadline bool
	enableCalls      int
	removeCalls      int
}

func (p *fakePlatform) Inspect(context.Context) (Inspection, error) {
	return p.inspection, p.inspectErr
}

func (p *fakePlatform) Enable(context.Context) error {
	p.enableCalls++
	if p.enableErr == nil && p.onEnable != nil {
		p.onEnable(p)
	}
	return p.enableErr
}

func (p *fakePlatform) Remove(context.Context) error {
	p.removeCalls++
	if p.removeErr == nil && p.onRemove != nil {
		p.onRemove(p)
	}
	return p.removeErr
}

func (p *blockingInspectionPlatform) Inspect(ctx context.Context) (Inspection, error) {
	_, p.receivedDeadline = ctx.Deadline()
	<-ctx.Done()
	return Inspection{}, ctx.Err()
}

func (p *blockingInspectionPlatform) Enable(context.Context) error {
	p.enableCalls++
	return nil
}

func (p *blockingInspectionPlatform) Remove(context.Context) error {
	p.removeCalls++
	return nil
}

func TestStatusIsReadOnlyAndSetupIsExplicit(t *testing.T) {
	platform := &fakePlatform{inspection: Inspection{
		Available: true,
		Reason:    ReasonCapabilityMissing,
		Message:   "setup needed",
	}}
	manager := NewManagerWithPlatform("test", platform)

	status := manager.Status(context.Background())
	if status.State != StateNeedsSetup || !status.CanEnable {
		t.Fatalf("Status = %+v, want an enableable needs_setup state", status)
	}
	if platform.enableCalls != 0 {
		t.Fatalf("read-only Status invoked Enable %d times", platform.enableCalls)
	}
}

func TestEnableIsIdempotentWhenAlreadyReady(t *testing.T) {
	platform := &fakePlatform{inspection: Inspection{
		Ready:      true,
		Configured: true,
		Reason:     ReasonReady,
		Message:    "ready",
	}}
	manager := NewManagerWithPlatform("test", platform)

	status, err := manager.Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if status.State != StateReady {
		t.Fatalf("status = %+v, want ready", status)
	}
	if platform.enableCalls != 0 {
		t.Fatalf("idempotent Enable invoked platform %d times", platform.enableCalls)
	}
}

func TestEnableVerifiesSuccessfulSetup(t *testing.T) {
	platform := &fakePlatform{
		inspection: Inspection{
			Available: true,
			Reason:    ReasonFirewallRuleMissing,
			Message:   "missing",
		},
		onEnable: func(p *fakePlatform) {
			p.inspection = Inspection{
				Ready:      true,
				Configured: true,
				Reason:     ReasonReady,
				Message:    "ready",
			}
		},
	}
	manager := NewManagerWithPlatform("test", platform)

	status, err := manager.Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if status.State != StateReady || platform.enableCalls != 1 {
		t.Fatalf("Enable status/calls = %+v / %d, want ready / 1", status, platform.enableCalls)
	}

	_, err = manager.Enable(context.Background())
	if err != nil {
		t.Fatalf("second Enable: %v", err)
	}
	if platform.enableCalls != 1 {
		t.Fatalf("second idempotent Enable changed calls to %d", platform.enableCalls)
	}
}

func TestEnableAcceptsLinuxRestartRequired(t *testing.T) {
	platform := &fakePlatform{
		inspection: Inspection{
			Available: true,
			Reason:    ReasonCapabilityMissing,
		},
		onEnable: func(p *fakePlatform) {
			p.inspection = Inspection{
				Available:  true,
				Configured: true,
				Reason:     ReasonRestartRequired,
				Message:    "quit and reopen",
			}
		},
	}
	manager := NewManagerWithPlatform("linux", platform)

	status, err := manager.Enable(context.Background())
	if err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if status.State != StateNeedsSetup || status.Reason != ReasonRestartRequired || status.CanEnable {
		t.Fatalf("status = %+v, want non-enableable restart_required", status)
	}
}

func TestAuthorizationDeclineDoesNotBecomeUnavailable(t *testing.T) {
	declined := errors.New("exit status 126")
	platform := &fakePlatform{
		inspection: Inspection{
			Available: true,
			Reason:    ReasonCapabilityMissing,
			Message:   "setup needed",
		},
		enableErr: &OperationError{
			Reason:  ReasonAuthorizationDeclined,
			Message: "authorization cancelled",
			Err:     declined,
		},
	}
	manager := NewManagerWithPlatform("linux", platform)

	status, err := manager.Enable(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("Enable error = %v, want cancellation", err)
	}
	if status.State != StateNeedsSetup || status.Reason != ReasonAuthorizationDeclined || !status.CanEnable {
		t.Fatalf("declined status = %+v, want retryable needs_setup", status)
	}
}

func TestUnsupportedFilesystemIsUnavailable(t *testing.T) {
	platform := &fakePlatform{
		inspection: Inspection{
			Available: true,
			Reason:    ReasonCapabilityMissing,
		},
		enableErr: &OperationError{
			Reason:      ReasonFilesystemUnsupported,
			Message:     "filesystem does not support capabilities",
			Unavailable: true,
		},
	}
	manager := NewManagerWithPlatform("linux", platform)

	status, err := manager.Enable(context.Background())
	if err == nil {
		t.Fatal("Enable unexpectedly succeeded")
	}
	if status.State != StateUnavailable || status.CanEnable || status.Reason != ReasonFilesystemUnsupported {
		t.Fatalf("failure status = %+v, want unavailable filesystem status", status)
	}
}

func TestEnableFailsSafeWhenVerificationDoesNotChange(t *testing.T) {
	platform := &fakePlatform{inspection: Inspection{
		Available: true,
		Reason:    ReasonFirewallRuleMissing,
		Message:   "still missing",
	}}
	manager := NewManagerWithPlatform("windows", platform)

	status, err := manager.Enable(context.Background())
	if err == nil || !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("Enable error = %v, want verification error", err)
	}
	if status.State != StateNeedsSetup || platform.enableCalls != 1 {
		t.Fatalf("status/calls = %+v / %d", status, platform.enableCalls)
	}
}

func TestRemoveIsIdempotentAndVerifies(t *testing.T) {
	platform := &fakePlatform{
		inspection: Inspection{
			Ready:      true,
			Configured: true,
			Reason:     ReasonReady,
		},
		onRemove: func(p *fakePlatform) {
			p.inspection = Inspection{
				Available: true,
				Reason:    ReasonFirewallRuleMissing,
				Message:   "removed",
			}
		},
	}
	manager := NewManagerWithPlatform("windows", platform)

	status, err := manager.Remove(context.Background())
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if status.State != StateNeedsSetup || status.CanRemove || platform.removeCalls != 1 {
		t.Fatalf("Remove status/calls = %+v / %d", status, platform.removeCalls)
	}

	_, err = manager.Remove(context.Background())
	if err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if platform.removeCalls != 1 {
		t.Fatalf("idempotent second Remove changed calls to %d", platform.removeCalls)
	}
}

func TestInspectionFailureIsUnavailableAndDoesNotElevate(t *testing.T) {
	platform := &fakePlatform{inspectErr: errors.New("inspection broke")}
	manager := NewManagerWithPlatform("test", platform)

	status, err := manager.Enable(context.Background())
	if err == nil {
		t.Fatal("Enable unexpectedly succeeded")
	}
	if status.State != StateUnavailable || status.Reason != ReasonInspectionFailed {
		t.Fatalf("status = %+v, want inspection failure", status)
	}
	if platform.enableCalls != 0 {
		t.Fatalf("inspection failure invoked Enable %d times", platform.enableCalls)
	}
}

func TestInspectionTimeoutBoundsStatusEnableAndRemove(t *testing.T) {
	tests := []struct {
		name string
		run  func(*Manager) Status
	}{
		{
			name: "status",
			run:  func(manager *Manager) Status { return manager.Status(context.Background()) },
		},
		{
			name: "enable preflight",
			run: func(manager *Manager) Status {
				status, _ := manager.Enable(context.Background())
				return status
			},
		},
		{
			name: "remove preflight",
			run: func(manager *Manager) Status {
				status, _ := manager.Remove(context.Background())
				return status
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform := &blockingInspectionPlatform{}
			manager := NewManagerWithPlatform("test", platform)
			manager.inspectionTimeout = 20 * time.Millisecond

			status := tt.run(manager)
			if !platform.receivedDeadline {
				t.Fatal("platform inspection did not receive a deadline")
			}
			if status.State != StateUnavailable || status.Reason != ReasonInspectionFailed ||
				!strings.Contains(status.Message, context.DeadlineExceeded.Error()) {
				t.Fatalf("status = %+v, want deadline-classified inspection failure", status)
			}
			if platform.enableCalls != 0 || platform.removeCalls != 0 {
				t.Fatalf(
					"timed-out inspection invoked mutation: enable=%d remove=%d",
					platform.enableCalls,
					platform.removeCalls,
				)
			}
		})
	}
}
