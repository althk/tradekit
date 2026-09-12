module github.com/althk/tradekit/go/store

go 1.25.0

require (
	github.com/althk/tradekit/go/core v0.1.0
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

// Until core is tagged, resolve it from the sibling directory. Drop this line
// (and rely on the tagged version above) when publishing store for external use.
replace github.com/althk/tradekit/go/core => ../core
