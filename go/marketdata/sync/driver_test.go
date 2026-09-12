//go:build sqlitedriver

package sync

// Registering a SQLite driver for the store-backed tests. See the identical
// file in go/store: no module here pins a driver, so a checkout with no network
// still builds and runs the driver-independent tests.
//
//	go test -tags sqlitedriver ./...
import _ "modernc.org/sqlite"
