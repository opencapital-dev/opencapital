#!/usr/bin/env python3
"""Generate the metric block of ``schemas/TOPICS.tsv`` from ``lib.streams.registry``.

Run this whenever ``ALL_STREAMS`` changes. The script prints the metric rows
to stdout — pipe into a sed-replace or copy into ``schemas/TOPICS.tsv``
between the existing ``# Metrics`` and ``# Entities`` headers.
"""

from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from lib.streams import ALL_STREAMS  # noqa: E402


def main() -> int:
    print("# Metrics (append-only, 30d retention) — generated, two rows per stream.")
    width_topic = max(len(s.recompute_topic) for s in ALL_STREAMS) + 4
    width_schema = max(len(s.schema_relpath) for s in ALL_STREAMS) + 4
    for s in ALL_STREAMS:
        for topic in s.all_topics():
            print(
                f"{topic:<{width_topic}}{s.schema_relpath:<{width_schema}}"
                f"{s.key_format:<8}{s.cleanup_policy}"
            )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
