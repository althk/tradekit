# backtest

Built. `Clock` (`RealClock`, `SimClock`), the `Replay` driver (feed → clock →
broker → strategy, in that order), the `Snapshotter` (mark-to-market equity
once per session), the `Recorder` (one run, one transaction, paper only), the
self-contained HTML `Report`, and the `Sweep` runner with `Windows` for
walk-forward splits.

There is deliberately no strategy interface and no fill model here. The loop
stays in the project; the fill rules live in `core/paper`, where live paper
trading and every backtest see one set of them. See
`specs/phase5_backtest/00_phase_note.md`.

## Verifying

```sh
cd go/backtest && GOWORK=off go test -tags sqlitedriver ./...
# regenerate the report golden file after an intended template change:
GOWORK=off go test -tags sqlitedriver -run Golden -update ./...
```
