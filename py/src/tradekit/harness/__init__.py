"""Operational plumbing every trading process needs and none should write again.

Mirrors ``go/harness``: configuration with secrets that cannot leak
(:mod:`.config`), log helpers that render domain types readably (:mod:`.obs`),
the decision journal (:mod:`.journal`), notifications that cannot stall the
trading loop (:mod:`.notify`) and the one callback server for the browser
login every Indian broker needs (:mod:`.login`). It owns no configuration schema and dictates no
logging stack.
"""

from . import config, journal, login, notify, obs
from .config import REDACTION, Secret, load, overlay, redacted
from .journal import Decision, Journal
from .login import Callback, browser_login
from .notify import Discard, Level, Multi, Notifier, Telegram

__all__ = [
    "REDACTION",
    "Callback",
    "Decision",
    "Discard",
    "Journal",
    "Level",
    "Multi",
    "Notifier",
    "Secret",
    "Telegram",
    "browser_login",
    "config",
    "journal",
    "load",
    "login",
    "notify",
    "obs",
    "overlay",
    "redacted",
]
