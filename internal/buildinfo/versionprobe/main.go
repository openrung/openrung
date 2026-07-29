// Command versionprobe prints the build identity linked into it, and exists
// only so injection_test.go can prove the -X symbol paths used by release
// builds still resolve. See internal/buildinfo/injection_test.go.
package main

import (
	"fmt"

	"openrung/internal/buildinfo"
)

func main() {
	fmt.Println(buildinfo.Info("probe", ""))
}
