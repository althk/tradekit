//go:build sqlitedriver

package harness

// Registering a SQLite driver for the store-backed tests.
//
// It lives behind a build tag for the same reason go/store's does: neither this
// module nor store pins a driver, so a consumer picks its own and a checkout
// with no network still builds and runs the driver-independent tests.
//
//	go test -tags sqlitedriver ./...
import _ "modernc.org/sqlite"
