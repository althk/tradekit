module github.com/althk/tradekit/go/harness

go 1.25.0

// Until core and store are tagged, resolve them from the sibling directories.
// Drop these lines when publishing harness externally.
replace github.com/althk/tradekit/go/core => ../core

replace github.com/althk/tradekit/go/store => ../store

require (
	github.com/BurntSushi/toml v1.5.0
	github.com/althk/tradekit/go/core v0.1.0
	github.com/althk/tradekit/go/store v0.0.0-00010101000000-000000000000
	modernc.org/sqlite v1.58.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.75.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)
