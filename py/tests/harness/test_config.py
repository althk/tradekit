"""Config loading, the environment overlay and Secret, including the shared parity block."""

from __future__ import annotations

import dataclasses
import datetime as dt
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import pytest

from tradekit.core import money
from tradekit.core.money import Money
from tradekit.harness import REDACTION, Secret, load, overlay, redacted

FIXTURE = Path(__file__).resolve().parents[3] / "contracts" / "testdata" / "parity.json"


# The shape the config parity block describes; go/harness/config_test.go
# declares the same one as a struct.
@dataclass
class Broker:  # noqa: D101
    api_key: Secret = field(metadata={"env": "TK_BROKER_API_KEY"})
    api_secret: Secret = field(default=Secret(""), metadata={"env": "TK_BROKER_API_SECRET"})


@dataclass
class Risk:  # noqa: D101
    max_positions: int = field(default=0, metadata={"env": "TK_RISK_MAX_POSITIONS"})
    risk_fraction: float = 0.0


@dataclass
class ParityConfig:  # noqa: D101
    name: str = field(metadata={"env": "TK_NAME"})
    broker: Broker = field(default_factory=lambda: Broker(api_key=Secret("")))
    risk: Risk = field(default_factory=Risk)


def _flat(cfg: ParityConfig) -> dict[str, Any]:
    return {
        "name": cfg.name,
        "broker.api_key": cfg.broker.api_key.reveal(),
        "broker.api_secret": cfg.broker.api_secret.reveal(),
        "risk.max_positions": cfg.risk.max_positions,
        "risk.risk_fraction": cfg.risk.risk_fraction,
    }


@pytest.fixture(scope="module")
def block() -> dict[str, Any]:
    return json.loads(FIXTURE.read_text(encoding="utf-8"))["config"]


def test_parity_config(block: dict[str, Any], tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    for case in block["cases"]:
        # Every variable the block names is cleared first, so a value left in
        # the developer's shell cannot decide the outcome.
        for name in block["env_names"].values():
            monkeypatch.delenv(name, raising=False)
        for k, v in case["env"].items():
            monkeypatch.setenv(k, v)
        path = tmp_path / "config.toml"
        path.write_text(case["toml"], encoding="utf-8")

        if case.get("want_missing"):
            with pytest.raises(ValueError) as exc:
                load(path, ParityConfig)
            for name in case["want_missing"]:
                assert name in str(exc.value), f"{case['name']}: the error must name every missing field"
            continue
        got = _flat(load(path, ParityConfig))
        assert got == case["want"], (
            f"{case['name']}: Go and Python must resolve the same file and environment identically"
        )


@dataclass
class Nested:  # noqa: D101
    ratio: float = field(default=0.0, metadata={"env": "SAMPLE_RATIO"})


@dataclass
class Sample:  # noqa: D101
    name: str = field(metadata={"env": "SAMPLE_NAME"})
    token: Secret = field(metadata={"env": "SAMPLE_TOKEN"})
    retries: int = field(metadata={"env": "SAMPLE_RETRIES"})
    timeout: dt.timedelta = field(default=dt.timedelta(0), metadata={"env": "SAMPLE_TIMEOUT"})
    limit: Money = field(default=money.ZERO, metadata={"env": "SAMPLE_LIMIT"})
    debug: bool = field(default=False, metadata={"env": "SAMPLE_DEBUG"})
    nested: Nested = field(default_factory=Nested)


def _write(tmp_path: Path, body: str) -> Path:
    path = tmp_path / "sample.toml"
    path.write_text(body, encoding="utf-8")
    return path


def test_env_override_wins_and_an_unset_variable_does_not_blank_the_field(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("SAMPLE_NAME", "")
    monkeypatch.setenv("SAMPLE_TOKEN", "from-env")
    monkeypatch.setenv("SAMPLE_RETRIES", "9")
    monkeypatch.setenv("SAMPLE_TIMEOUT", "1m30s")
    monkeypatch.setenv("SAMPLE_LIMIT", "2000.50")
    monkeypatch.setenv("SAMPLE_DEBUG", "true")
    monkeypatch.setenv("SAMPLE_RATIO", "0.25")
    cfg = load(_write(tmp_path, 'name = "from-file"\ntoken = "file-token"\nretries = 2\n'), Sample)
    assert cfg.name == "from-file", "an unset variable must leave the file's value alone"
    assert cfg.token.reveal() == "from-env"
    assert cfg.retries == 9 and cfg.timeout == dt.timedelta(seconds=90) and cfg.debug is True
    assert cfg.limit == 200050, "a Money override is a decimal string, parsed to minor units"
    assert cfg.nested.ratio == 0.25
    assert isinstance(cfg.token, Secret), "a Secret field stays a Secret whichever source filled it"


def test_money_rejects_sub_minor_precision_and_bare_numbers(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SAMPLE_LIMIT", "2000.505")
    with pytest.raises(ValueError, match="SAMPLE_LIMIT"):
        load(_write(tmp_path, 'name = "x"\ntoken = "y"\nretries = 1\n'), Sample)
    monkeypatch.delenv("SAMPLE_LIMIT")
    with pytest.raises(ValueError, match="decimal string"):
        overlay({"name": "n", "token": "t", "retries": 1, "limit": 2000}, Sample)
    cfg = overlay({"name": "n", "token": "t", "retries": 1, "limit": "2000.00"}, Sample)
    assert cfg.limit == 200000, "a decimal string in the file is parsed the same way as one from the environment"


def test_an_unparseable_override_names_the_variable(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("SAMPLE_RETRIES", "many")
    with pytest.raises(ValueError, match="SAMPLE_RETRIES"):
        load(_write(tmp_path, 'name = "x"\ntoken = "y"\nretries = 1\n'), Sample)


def test_missing_required_fields_are_all_named_in_one_error(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    for name in ("SAMPLE_NAME", "SAMPLE_TOKEN", "SAMPLE_RETRIES"):
        monkeypatch.delenv(name, raising=False)
    with pytest.raises(ValueError) as exc:
        load(_write(tmp_path, "debug = true\n"), Sample)
    for want in ("name", "token", "retries"):
        assert want in str(exc.value), "one restart must be enough to learn every missing field"


def test_an_unknown_key_is_an_error(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("SAMPLE_NAME", raising=False)
    with pytest.raises(ValueError, match="unknown config key 'nmae'"):
        load(_write(tmp_path, 'nmae = "typo"\ntoken = "y"\nretries = 1\n'), Sample)


def test_load_needs_a_dataclass() -> None:
    with pytest.raises(TypeError):
        load("nowhere.toml", dict)  # type: ignore[type-var]


def test_overlay_without_a_file(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("SAMPLE_TIMEOUT", raising=False)
    cfg = overlay({"name": "n", "token": "t", "retries": 1, "timeout": "2h"}, Sample)
    assert cfg.timeout == dt.timedelta(hours=2), "a Go-style duration in the file is accepted, as Go accepts it"


@dataclass
class Creds:  # noqa: D101
    user: str
    password: Secret


def test_secret_is_redacted_through_every_default_path() -> None:
    c = Creds(user="ops", password=Secret("hunter2"))
    for name, rendered in {
        "repr": repr(c),
        "str": str(c.password),
        "f-string": f"{c.password}",
        "format": "{0}".format(c.password),  # noqa: UP030, UP032 - the explicit form is the point
        "percent": "%s" % c.password,  # noqa: UP031 - likewise
        "redacted": redacted(c),
        "json of asdict": json.dumps(dataclasses.asdict(c), default=str),
    }.items():
        assert "hunter2" not in rendered, f"{name} leaked the secret: {rendered}"
        assert REDACTION in rendered, f"{name} must show the marker: {rendered}"
    assert c.password.reveal() == "hunter2"


def test_secret_is_not_a_string_so_the_buffer_paths_cannot_leak() -> None:
    # A str subclass would stay redacted through repr and f-strings while
    # str.join and json.dumps read the buffer. The wrapper refuses instead.
    s = Secret("hunter2")
    with pytest.raises(TypeError):
        ",".join([s])  # type: ignore[list-item]
    with pytest.raises(TypeError):
        json.dumps({"password": s})
    assert s == Secret("hunter2") and s != "hunter2", "a secret never compares equal to a plain string"
    assert bool(Secret("")) is False and bool(s) is True


def test_redacted_walks_nested_containers() -> None:
    @dataclass
    class Outer:
        inner: Creds
        extras: dict[str, Any]
        keys: list[Secret]

    out = redacted(Outer(Creds("u", Secret("hunter2")), {"pw": Secret("hunter3"), "n": 1}, [Secret("hunter4")]))
    assert "hunter" not in out, out
    assert "inner.user='u'" in out and "extras.n=1" in out, out
