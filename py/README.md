# tradekit (Python)

One distribution mirroring the Go modules package for package: `core` (domain,
money, ports, risk, costs, calendar, indicators, stats, paper), `store`,
`marketdata`, `harness`, plus vendor adapters `upstox` and `fyers`. Not
published to PyPI — install straight from the repo.

```sh
pip install "tradekit @ git+https://github.com/althk/tradekit.git#subdirectory=py"
```

Pin to a tag instead of the branch head once the repo has releases:

```sh
pip install "tradekit @ git+https://github.com/althk/tradekit.git@v0.1.0#subdirectory=py"
```

Vendor SDKs are optional extras, so a screener that only needs indicators and
costs installs nothing beyond the standard library:

```sh
pip install "tradekit[upstox] @ git+https://github.com/althk/tradekit.git#subdirectory=py"
```

Extras: `zerodha`, `upstox`, `fyers`, `alpaca`, `pandas` (the `marketdata`
frame bridge), `dev` (test and lint tooling for working on this repo itself).

```python
from tradekit.core import money, ports
from tradekit.upstox import UpstoxBroker
```

## Layout

```text
src/tradekit/
  core/         mirrors go/core module for module, paper included
  store/        applies the same contracts/sqlite schema
  upstox/       mirrors go/upstox
  fyers/        mirrors go/fyers
  marketdata/   mirrors go/marketdata; frames.py is the optional pandas bridge
  harness/      config + Secret, logging helpers, decision journal, notifiers
```

## Running the tests

```sh
pip install -e ".[dev]"
ruff check src tests
ruff format --check src tests
pytest -q
```

Both suites run the same `contracts/testdata/parity.json`; see the top-level
README for what that guarantees against the Go side.
