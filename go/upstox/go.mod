module github.com/althk/tradekit/go/upstox

go 1.24

// Until core is tagged, resolve it from the sibling directory. Drop this line
// (and rely on the tagged version above) when publishing upstox externally.
replace github.com/althk/tradekit/go/core => ../core

require github.com/althk/tradekit/go/core v0.0.0-00010101000000-000000000000
