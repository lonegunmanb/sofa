"""External sofa AgentHarness plugin for Google Antigravity."""

from __future__ import annotations

import sys
from pathlib import Path

_PROTO_ROOT = str(Path(__file__).resolve().parent / "proto")
if _PROTO_ROOT not in sys.path:
    sys.path.insert(0, _PROTO_ROOT)

__all__ = ["__version__"]
__version__ = "0.1.0"
