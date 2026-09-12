//go:build sqlitedriver

package store

// Registering a SQLite driver for the integration tests.
//
// It lives behind a build tag so the store module itself keeps zero
// third-party dependencies: consumers pick their own driver, and a checkout
// with no network still builds and runs the driver-independent tests.
//
//	go test -tags sqlitedriver ./...
//
// The CI job runs with the tag; a local run without it skips the SQL tests
// and says so.
import _ "modernc.org/sqlite"
