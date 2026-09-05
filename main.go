// Command proxio relays an HTTP request to a URL somebody else chose.
package main

import (
	"os"
	"time"

	"proxio/internal/cli"
)

func main() {
	// Every time this logs is UTC, whatever TZ says. The image carries no zone database, so
	// a named TZ would resolve to UTC regardless — pinning it makes that a decision rather
	// than a side effect of what the runtime happens to contain.
	time.Local = time.UTC
	os.Exit(cli.Execute())
}
