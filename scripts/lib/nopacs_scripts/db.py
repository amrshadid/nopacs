"""A catalog read by the database's own library, as an outside check does:
read only, so a proof cannot change what it judges."""

import sqlite3


def query(db_path, sql, *params):
    """Rows of a SQLite database, read by SQLite itself, read only; each row
    answers by column name."""
    db = sqlite3.connect("file:%s?mode=ro" % db_path, uri=True, timeout=30)
    db.row_factory = sqlite3.Row
    try:
        return db.execute(sql, params).fetchall()
    finally:
        db.close()
