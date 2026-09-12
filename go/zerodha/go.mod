module github.com/althk/tradekit/go/zerodha

go 1.24

// Until core is tagged, resolve it from the sibling directory. Drop this line
// (and rely on the tagged version above) when publishing zerodha externally.
replace github.com/althk/tradekit/go/core => ../core

require (
	github.com/althk/tradekit/go/core v0.0.0-00010101000000-000000000000
	github.com/zerodha/gokiteconnect/v4 v4.4.2
)

require (
	github.com/gocarina/gocsv v0.0.0-20180809181117-b8c38cb1ba36 // indirect
	github.com/google/go-querystring v1.0.0 // indirect
	github.com/gorilla/websocket v1.4.2 // indirect
)
