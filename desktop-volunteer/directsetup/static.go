package directsetup

import "context"

type staticPlatform struct {
	inspection Inspection
}

func (p staticPlatform) Inspect(context.Context) (Inspection, error) { return p.inspection, nil }
func (staticPlatform) Enable(context.Context) error                  { return nil }
func (staticPlatform) Remove(context.Context) error                  { return nil }
