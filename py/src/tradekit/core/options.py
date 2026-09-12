"""Black-Scholes for the two things a strategy asks of it. Mirrors ``go/core/options``.

Used for one job: choosing a strike by delta rather than by a fixed number of
index points. The two are not interchangeable -- 150 points ITM on a NIFTY
weekly is roughly 0.95 delta on expiry morning and roughly 0.68 delta six days
out, because what sets delta is the move in units of sigma*sqrt(T), not in
points. Holding the point offset fixed silently sweeps delta across the week.

Volatility is never assumed: it is backed out of the at-the-money premium that
actually traded. The carry terms are parameters of a :class:`Model` rather
than constants, for the same reason no rate card ships in code.
"""

from __future__ import annotations

import datetime as dt
import math
from dataclasses import dataclass

__all__ = ["MIN_YEARS", "Model", "years_to_expiry"]

#: Floors time to expiry at about ten minutes. Delta is a step function in the
#: last moments before expiry and the inversion stops being meaningful; the
#: floor keeps both finite rather than pretending to a precision that is not
#: there.
MIN_YEARS = 10.0 / (60 * 24 * 365)

_A = (
    -3.969683028665376e01,
    2.209460984245205e02,
    -2.759285104469687e02,
    1.383577518672690e02,
    -3.066479806614716e01,
    2.506628277459239e00,
)
_B = (-5.447609879822406e01, 1.615858368580409e02, -1.556989798598866e02, 6.680131188771972e01, -1.328068155288572e01)
_C = (
    -7.784894002430293e-03,
    -3.223964580411365e-01,
    -2.400758277161838e00,
    -2.549732539343734e00,
    4.374664141464968e00,
    2.938163982698783e00,
)
_D = (7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e00, 3.754408661907416e00)


def _norm_cdf(x: float) -> float:
    return 0.5 * math.erfc(-x / math.sqrt(2))


def _norm_inv_cdf(p: float) -> float:
    """Acklam's rational approximation, ~1e-9 relative accuracy."""
    if p <= 0 or p >= 1:
        return math.nan
    plow, phigh = 0.02425, 1 - 0.02425
    if p < plow:
        q = math.sqrt(-2 * math.log(p))
        return (((((_C[0] * q + _C[1]) * q + _C[2]) * q + _C[3]) * q + _C[4]) * q + _C[5]) / (
            (((_D[0] * q + _D[1]) * q + _D[2]) * q + _D[3]) * q + 1
        )
    if p > phigh:
        q = math.sqrt(-2 * math.log(1 - p))
        return -(((((_C[0] * q + _C[1]) * q + _C[2]) * q + _C[3]) * q + _C[4]) * q + _C[5]) / (
            (((_D[0] * q + _D[1]) * q + _D[2]) * q + _D[3]) * q + 1
        )
    q = p - 0.5
    r = q * q
    return (
        (((((_A[0] * r + _A[1]) * r + _A[2]) * r + _A[3]) * r + _A[4]) * r + _A[5])
        * q
        / (((((_B[0] * r + _B[1]) * r + _B[2]) * r + _B[3]) * r + _B[4]) * r + 1)
    )


def _intrinsic(s: float, k: float, is_call: bool) -> float:
    return s - k if is_call else k - s


@dataclass(frozen=True, slots=True)
class Model:
    """The carry terms in d1.

    Over a holding period of hours on a weekly option their effect on the
    chosen strike is a point or two; they are here for correctness, not
    because the result turns on them.
    """

    risk_free: float
    """Continuously compounded risk-free rate, as a fraction."""
    div_yield: float
    """The underlying's dividend yield, as a fraction."""

    def _d1(self, s: float, k: float, t: float, sigma: float) -> float:
        return (math.log(s / k) + (self.risk_free - self.div_yield + sigma * sigma / 2) * t) / (sigma * math.sqrt(t))

    def price(self, s: float, k: float, t: float, sigma: float, is_call: bool) -> float:
        """Value a European call or put on an index."""
        if t <= 0 or sigma <= 0 or s <= 0 or k <= 0:
            return max(0.0, _intrinsic(s, k, is_call))
        a = self._d1(s, k, t, sigma)
        b = a - sigma * math.sqrt(t)
        df_r, df_q = math.exp(-self.risk_free * t), math.exp(-self.div_yield * t)
        if is_call:
            return s * df_q * _norm_cdf(a) - k * df_r * _norm_cdf(b)
        return k * df_r * _norm_cdf(-b) - s * df_q * _norm_cdf(-a)

    def delta(self, s: float, k: float, t: float, sigma: float, is_call: bool) -> float:
        """The option's delta: positive for a call, negative for a put."""
        if t <= 0 or sigma <= 0:
            if _intrinsic(s, k, is_call) > 0:
                return 1.0 if is_call else -1.0
            return 0.0
        dq = math.exp(-self.div_yield * t)
        if is_call:
            return dq * _norm_cdf(self._d1(s, k, t, sigma))
        return -dq * _norm_cdf(-self._d1(s, k, t, sigma))

    def implied_vol(self, price: float, s: float, k: float, t: float, is_call: bool) -> float | None:
        """Invert :meth:`price` for sigma by bisection.

        Bisection rather than Newton because vega collapses on a deep-ITM or
        nearly-expired option and Newton diverges exactly where this is asked
        the hardest; price is monotone in sigma, so bisection always converges.

        Returns ``None`` when the observed price is outside the arbitrage
        bounds the model can reproduce -- a stale or crossed print, which must
        not be papered over with a made-up vol.
        """
        if t <= 0 or price <= 0 or s <= 0 or k <= 0:
            return None
        lo, hi = 0.005, 5.0
        if price <= self.price(s, k, t, lo, is_call) or price >= self.price(s, k, t, hi, is_call):
            return None
        for _ in range(100):
            mid = (lo + hi) / 2
            if self.price(s, k, t, mid, is_call) < price:
                lo = mid
            else:
                hi = mid
            if hi - lo < 1e-6:
                break
        return (lo + hi) / 2

    def strike_for_delta(self, s: float, t: float, sigma: float, target: float, is_call: bool) -> float:
        """The strike whose delta has the requested magnitude.

        Lands below spot for a call and above it for a put -- in the money in
        both cases, as intended.
        """
        t = max(t, MIN_YEARS)
        if sigma <= 0 or target <= 0 or target >= 1:
            return s
        z = _norm_inv_cdf(target)
        if not is_call:
            z = -z
        return s * math.exp(-z * sigma * math.sqrt(t) + (self.risk_free - self.div_yield + sigma * sigma / 2) * t)


def years_to_expiry(now: dt.datetime, expiry_close: dt.datetime) -> float:
    """Calendar years from ``now`` to the instant the contract dies, floored at :data:`MIN_YEARS`.

    ``expiry_close`` is the close of the expiry session; build it with the
    exchange's session so the hours stay in one place.
    """
    years = (expiry_close - now).total_seconds() / 3600 / (24 * 365)
    return max(years, MIN_YEARS)
