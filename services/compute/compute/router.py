"""Route an auto-store query to rw or pg by the tables it reads."""
from __future__ import annotations
import sqlglot
from sqlglot import exp

def tables_in(sql: str) -> set[str]:
    """Bare table names referenced by *sql* (schema/db qualifiers stripped, lowercased).

    CTE aliases are excluded: ``WITH cte AS (...) SELECT * FROM cte`` returns
    only the tables referenced inside the CTE body, not ``cte`` itself.
    """
    try:
        tree = sqlglot.parse_one(sql, read="postgres")
    except Exception as exc:
        raise ValueError(f"unparseable SQL: {exc}") from exc
    ctes = {c.alias_or_name.lower() for c in tree.find_all(exp.CTE)}
    return {t.name.lower() for t in tree.find_all(exp.Table) if t.name and t.name.lower() not in ctes}

# The lone CDC-mirrored table; on auto it resolves to the RW analytics mirror.
_OVERLAP_DEFAULT = {"portfolios": "rw"}

def decide_store(tables: set[str], catalog: dict[str, str]) -> str:
    """Pick "rw" or "pg" for a query reading *tables*; raise on unknown/cross-store."""
    stores: set[str] = set()
    for t in tables:
        loc = catalog.get(t)
        if loc is None:
            raise ValueError(f"table {t!r} not found in rw or pg catalog")
        if loc == "both":
            stores.add(_OVERLAP_DEFAULT.get(t, "rw"))
        else:
            stores.add(loc)
    if len(stores) > 1:
        raise ValueError(f"query reads across stores {sorted(stores)}; cross-store joins unsupported")
    return stores.pop() if stores else "rw"
