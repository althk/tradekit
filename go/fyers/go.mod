module github.com/althk/tradekit/go/fyers

go 1.24

// Until core is tagged, resolve it from the sibling directory. Drop this line
// (and rely on the tagged version above) when publishing fyers externally.
replace github.com/althk/tradekit/go/core => ../core

require (
	github.com/FyersDev/fyers-go-sdk v1.6.0
	github.com/althk/tradekit/go/core v0.0.0-00010101000000-000000000000
)

require github.com/gorilla/websocket v1.5.3 // indirect
