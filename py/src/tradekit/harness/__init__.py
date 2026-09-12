"""Operational plumbing every trading process needs and none should write again.

Mirrors ``go/harness``: configuration with secrets that cannot leak
(:mod:`.config`), log helpers that render domain types readably (:mod:`.obs`),
the decision journal (:mod:`.journal`) and notifications that cannot stall the
trading loop (:mod:`.notify`). It owns no configuration schema and dictates no
logging stack.
"""

from . import config, journal, notify, obs
from .config import REDACTION, Secret, load, overlay, redacted
from .journal import Decision, Journal
from .notify import Discard, Level, Multi, Notifier, Telegram

__all__ = [
    "REDACTION",
    "Decision",
    "Discard",
    "Journal",
    "Level",
    "Multi",
    "Notifier",
    "Secret",
    "Telegram",
    "config",
    "journal",
    "load",
    "notify",
    "obs",
    "overlay",
    "redacted",
]
