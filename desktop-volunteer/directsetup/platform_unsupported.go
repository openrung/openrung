//go:build !windows && !linux && !darwin

package directsetup

import "context"

type unsupportedPlatform struct{}

func newPlatform() Platform { return unsupportedPlatform{} }

func (unsupportedPlatform) Inspect(context.Context) (Inspection, error) {
	return Inspection{
		Reason:  ReasonUnsupportedPlatform,
		Message: "This operating system does not have an OpenRung TCP 443 setup integration.",
	}, nil
}

func (unsupportedPlatform) Enable(context.Context) error { return nil }
func (unsupportedPlatform) Remove(context.Context) error { return nil }
