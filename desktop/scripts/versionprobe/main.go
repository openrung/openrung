// Command versionprobe prints the app version linked into it, and exists only
// so CI can prove the -X symbol path in scripts/versioned-wails-build.mjs
// still resolves. See scripts/version-injection.test.mjs.
package main

import (
	"fmt"

	"openrung/internal/client"
)

func main() {
	fmt.Println(client.AppVersion())
}
