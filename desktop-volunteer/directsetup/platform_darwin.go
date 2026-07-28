//go:build darwin

package directsetup

import "context"

// macOS intentionally has no sudo/setuid fallback. A privileged listener must
// be supplied by a minimal, signed SMAppService helper with authenticated IPC.
// The current release artifact is ad-hoc signed, so the helper cannot yet be
// installed and updated with an identity macOS can authenticate.
type darwinPlatform struct{}

func newPlatform() Platform { return darwinPlatform{} }

func (darwinPlatform) Inspect(context.Context) (Inspection, error) {
	return Inspection{
		Reason: ReasonReleaseSigningRequired,
		Message: "TCP 443 setup on macOS requires a minimal SMAppService privileged helper, " +
			"a stable Developer ID signature, and notarized packaging. This build is ad-hoc signed, " +
			"so OpenRung will not request sudo or install an unauthenticated helper. Automatic mode " +
			"continues with the alternate port and RelayHub.",
	}, nil
}

func (darwinPlatform) Enable(context.Context) error {
	return &OperationError{
		Reason:      ReasonReleaseSigningRequired,
		Unavailable: true,
		Message:     "A securely signed SMAppService helper is not available in this build.",
	}
}

func (darwinPlatform) Remove(context.Context) error { return nil }
