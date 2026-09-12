# marketdata

Built. `feed` (the `BarFeed` port), `sync` (chunking, gap-fill, cadence,
resume), `universe` (NSE index constituents and bhavcopy), `reference`
(holidays, corporate actions) and `bars` (bar construction and sessions), plus
`ExportCSV` for a common `histdl`-style on-disk layout.

The Python mirror is `py/src/tradekit/marketdata/`, module for module. Both
run the `marketdata` block of `contracts/testdata/parity.json` (chunk
boundaries, resampling, corporate-action adjustment).

See `../../tradekit-design.md` for what belongs here and which existing
project code is its donor.

## Verifying

```sh
cd go/marketdata && GOWORK=off go test -tags sqlitedriver ./...
```
