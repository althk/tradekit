"""The bar, universe and reference-data layer every backtest and every sync reads through.

Mirrors ``go/marketdata``. It replaces four incompatible ways of reading the
same bars: zerobha from a CSV tree, neev and breakout500 from SQLite, conflux
from pandas pickles. One port over three sources ends that.

    from tradekit.marketdata import from_store, bars, sync, reference, universe

The submodules mirror the Go packages of the same name: :mod:`.sync` for
chunking, gap-fill, cadence and resume; :mod:`.universe` for NSE index lists
and the bhavcopy; :mod:`.reference` for tick and lot sizes, the holiday cache
and corporate actions; :mod:`.bars` for tick aggregation, resampling and
session volume. :mod:`.frames` is the optional pandas bridge and is not
imported here, so nothing in the core paths needs pandas.
"""

from . import bars, reference, sync, universe
from .feed import (
    CSV_HEADER,
    IST,
    BarFeed,
    SliceFeed,
    csv_path,
    export_csv,
    from_csv,
    from_slice,
    from_store,
    parse_csv_time,
    read_csv,
)

__all__ = [
    "CSV_HEADER",
    "IST",
    "BarFeed",
    "SliceFeed",
    "bars",
    "csv_path",
    "export_csv",
    "from_csv",
    "from_slice",
    "from_store",
    "parse_csv_time",
    "read_csv",
    "reference",
    "sync",
    "universe",
]
