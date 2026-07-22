package wsscore

import (
	"fmt"
	"time"
)

func boundedInt(value, fallback, maximum int, name string) (int, error) {
	if value == 0 {
		value = fallback
	}
	if value < 1 || value > maximum {
		return 0, fmt.Errorf("%s must be within [1, %d]", name, maximum)
	}
	return value, nil
}

func boundedDuration(value, fallback, maximum time.Duration, name string) (time.Duration, error) {
	if value == 0 {
		value = fallback
	}
	if value < time.Millisecond || value > maximum {
		return 0, fmt.Errorf("%s must be within [1ms, %s]", name, maximum)
	}
	return value, nil
}
