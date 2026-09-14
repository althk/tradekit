"""Configuration loading: a TOML file, an environment overlay, and secrets that cannot leak.

Mirrors ``go/harness/config.go``. The mechanism differs -- dataclasses and
:mod:`tomllib` rather than struct tags -- but the behaviour must not, because a
Go project and a Python project sharing a deployment read the same file the
same way. This module owns no schema: the dataclass is the caller's.

Environment overrides come from ``field(metadata={"env": "KITE_API_KEY"})``.
Required-ness comes from a field having no default, which is already how
dataclasses express it. A missing required field fails at load, not at first
use, and every missing field is named in one error.
"""

from __future__ import annotations

import datetime as dt
import os
import tomllib
import types
import typing
from dataclasses import MISSING, fields, is_dataclass
from pathlib import Path
from typing import Any, TypeVar

from ..core import money
from ..core.money import Money

__all__ = ["REDACTION", "Secret", "load", "overlay", "redacted"]

T = TypeVar("T")

REDACTION = "<redacted>"
"""What a :class:`Secret` renders as everywhere but :meth:`Secret.reveal`."""


class Secret:
    """A credential that renders as ``<redacted>``.

    Credentials reach logs through ``repr`` of a containing config object.
    Making every credential field this type means the default path is safe:
    ``repr``, ``str``, f-strings, :func:`redacted` and :func:`json.dumps` with
    ``default=str`` all show the marker, and :func:`json.dumps` without it
    refuses rather than leaks.

    It is a wrapper, not a :class:`str` subclass, deliberately. A subclass
    stays redacted through ``repr`` and f-strings but :meth:`str.join` and
    :func:`json.dumps` read the buffer rather than calling ``__str__``, so a
    status endpoint serialising its config would publish the credential. The
    cost is that :meth:`reveal` is mandatory at every use site -- which is
    also what makes every use site greppable, and is how Go's ``Secret``
    works too.
    """

    __slots__ = ("_value",)

    def __init__(self, value: str = "") -> None:
        """Wrap the real value."""
        self._value = str(value)

    def __repr__(self) -> str:
        """The redaction marker."""
        return REDACTION

    def __str__(self) -> str:
        """The redaction marker."""
        return REDACTION

    def __format__(self, spec: str) -> str:
        """The redaction marker, so f-strings and :meth:`str.format` are safe."""
        return format(REDACTION, spec)

    def __bool__(self) -> bool:
        """Whether a value is set, so ``if not token`` reads naturally."""
        return bool(self._value)

    def __eq__(self, other: object) -> bool:
        """Equal to another Secret with the same value; never to a plain string."""
        return isinstance(other, Secret) and other._value == self._value

    def __hash__(self) -> int:
        """Hash by value."""
        return hash(self._value)

    def reveal(self) -> str:
        """The actual value. The only way to read one."""
        return self._value


def load(path: str | Path, cls: type[T]) -> T:
    """Decode a TOML file into a dataclass, then apply environment overrides.

    Nested dataclasses map to TOML tables. A field tagged
    ``metadata={"env": NAME}`` takes that variable's value when it is set and
    non-empty; an unset or empty variable leaves the file's value alone, so a
    deployment can override one field without restating the file.

    Raises:
        ValueError: On an unreadable value, an unknown key, or -- listing every
            one of them -- a missing required field.

    """
    if not is_dataclass(cls) or not isinstance(cls, type):
        raise TypeError(f"harness: load needs a dataclass type, got {cls!r}")
    with Path(path).open("rb") as f:
        data = tomllib.load(f)
    return overlay(data, cls)


def overlay(data: dict[str, Any], cls: type[T]) -> T:
    """Build ``cls`` from already-decoded data plus the environment.

    :func:`load` calls this; a caller with its own source of the mapping can
    call it directly.
    """
    missing: list[str] = []
    value = _build(data, cls, "", missing)
    if missing:
        raise ValueError(f"harness: missing required config: {', '.join(sorted(missing))}")
    return value


def _build(data: dict[str, Any], cls: type[Any], prefix: str, missing: list[str]) -> Any:
    hints = typing.get_type_hints(cls)
    known = {f.name for f in fields(cls)}
    for key in data:
        if key not in known:
            raise ValueError(f"harness: unknown config key {prefix + key!r}")

    kwargs: dict[str, Any] = {}
    for f in fields(cls):
        name = prefix + f.name
        hint = _unwrap_optional(hints.get(f.name, f.type))
        present = f.name in data
        raw = data.get(f.name)

        if isinstance(hint, type) and is_dataclass(hint):
            sub = raw if present else {}
            if not isinstance(sub, dict):
                raise ValueError(f"harness: {name} must be a table")
            kwargs[f.name] = _build(sub, hint, name + ".", missing)
            continue

        value: Any = _coerce(raw, hint, name) if present else MISSING
        env = f.metadata.get("env")
        if env:
            override = os.environ.get(env, "")
            if override != "":
                try:
                    value = _parse(override, hint)
                except ValueError as exc:
                    raise ValueError(f"harness: {name} from ${env}: {exc}") from None

        if value is MISSING:
            if f.default is not MISSING:
                value = f.default
            elif f.default_factory is not MISSING:
                value = f.default_factory()
            else:
                missing.append(name)
                continue
        kwargs[f.name] = value
    if missing:
        # The caller raises once with the whole list; return a placeholder so
        # nested builds can keep collecting.
        return None
    return cls(**kwargs)


def _unwrap_optional(hint: Any) -> Any:
    origin = typing.get_origin(hint)
    if origin is typing.Union or origin is types.UnionType:
        args = [a for a in typing.get_args(hint) if a is not type(None)]
        if len(args) == 1:
            return args[0]
    return hint


def _coerce(raw: Any, hint: Any, name: str) -> Any:
    """Apply the field's type to a TOML value: Secret wraps, Duration and Money parse, else as read."""
    if hint is Secret:
        return Secret(str(raw))
    if hint is dt.timedelta and isinstance(raw, str):
        return _parse(raw, hint)
    if hint is Money:
        # Money is read from a decimal string, never a bare number: 2000 in
        # a file means two thousand rupees to whoever typed it, and a limit
        # that silently became twenty would not be noticed until it tripped.
        if not isinstance(raw, str):
            raise ValueError(f'harness: {name} must be a decimal string such as "2000.00", got {raw!r}')
        return _parse(raw, hint)
    if hint is float and isinstance(raw, int) and not isinstance(raw, bool):
        return float(raw)
    return raw


def _parse(raw: str, hint: Any) -> Any:
    """Parse an environment string into the field's type."""
    if hint is Secret:
        return Secret(raw)
    if hint is str or hint is Any:
        return raw
    if hint is bool:
        lowered = raw.strip().lower()
        if lowered in ("1", "t", "true", "yes", "y", "on"):
            return True
        if lowered in ("0", "f", "false", "no", "n", "off"):
            return False
        raise ValueError(f"{raw!r} is not a boolean")
    if hint is int:
        return int(raw)
    if hint is float:
        return float(raw)
    if hint is dt.timedelta:
        return _duration(raw)
    if hint is Money:
        return money.parse(raw)
    raise ValueError(f"unsupported field type {hint!r} for an env override")


_UNITS = {"ms": 0.001, "s": 1.0, "m": 60.0, "h": 3600.0, "d": 86400.0}


def _duration(raw: str) -> dt.timedelta:
    """Read a Go-style duration such as ``1m30s``, so both languages accept the same value."""
    total = 0.0
    number = ""
    i = 0
    text = raw.strip()
    while i < len(text):
        ch = text[i]
        if ch.isdigit() or ch == ".":
            number += ch
            i += 1
            continue
        unit = ""
        while i < len(text) and text[i].isalpha():
            unit += text[i]
            i += 1
        if not number or unit not in _UNITS:
            raise ValueError(f"{raw!r} is not a duration")
        total += float(number) * _UNITS[unit]
        number = ""
    if number or not text:
        raise ValueError(f"{raw!r} is not a duration")
    return dt.timedelta(seconds=total)


def redacted(cfg: Any) -> str:
    """Render a config with :class:`Secret` fields masked, one ``path=value`` per field.

    The only supported way to log one. It walks by type, so a Secret nested in
    a plain dict or list is masked too, not only a top-level dataclass field.
    """
    parts: list[str] = []
    _render(cfg, "", parts)
    return " ".join(parts)


def _render(value: Any, prefix: str, parts: list[str]) -> None:
    if is_dataclass(value) and not isinstance(value, type):
        for f in fields(value):
            _render(getattr(value, f.name), f"{prefix}{f.name}.", parts)
        return
    key = prefix.rstrip(".")
    if isinstance(value, Secret):
        parts.append(f"{key}={REDACTION}")
    elif isinstance(value, dict):
        for k, v in value.items():
            _render(v, f"{key}.{k}.", parts)
    elif isinstance(value, list | tuple):
        parts.append(f"{key}=[{', '.join(REDACTION if isinstance(v, Secret) else repr(v) for v in value)}]")
    else:
        parts.append(f"{key}={value!r}")
