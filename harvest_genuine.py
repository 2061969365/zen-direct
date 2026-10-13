#!/usr/bin/env python3
"""harvest_genuine.py: harvest backend-known opencode session IDs for zen-direct.

Upstream FreeTier gate allowlists X-Opencode-Session values it has seen from a
genuine opencode client; locally minted ses_ IDs get 403 FreeTierError. This
script dumps genuine session IDs (models with providerID "opencode", i.e. ones
that actually called Zen) from the local opencode.db into genuine_sessions.txt
(one per line, most-recent first), which zen-direct's pool uses instead of
minting fakes.

Usage:
    python harvest_genuine.py [out_path]
    python harvest_genuine.py --db PATH --out PATH --limit 512

Read-only open (mode=ro) so a running opencode service is never locked.
Best-effort: failures print to stderr and exit 1; callers must tolerate that
(zen-direct falls back to minting, which will 403 on free tier).
"""
import os
import sqlite3
import sys

DEFAULT_LIMIT = 512


def find_db(explicit=""):
    if explicit:
        return explicit
    home = os.path.expanduser("~")
    return os.path.join(home, ".local", "share", "opencode", "opencode.db")


def harvest(db_path, limit):
    uri = "file:{}?mode=ro".format(db_path.replace("?", "%3F"))
    con = sqlite3.connect(uri, uri=True, timeout=5)
    try:
        rows = con.execute(
            "select id from session where model like '%\"providerID\":\"opencode\"%' "
            "order by time_updated desc limit ?",
            (limit,),
        ).fetchall()
    finally:
        con.close()
    ids = []
    seen = set()
    for (sid,) in rows:
        if isinstance(sid, str) and sid.startswith("ses_") and sid not in seen:
            seen.add(sid)
            ids.append(sid)
    return ids


def main(argv):
    db = ""
    out = ""
    limit = DEFAULT_LIMIT
    i = 0
    while i < len(argv):
        a = argv[i]
        if a == "--db" and i + 1 < len(argv):
            db = argv[i + 1]
            i += 2
        elif a == "--out" and i + 1 < len(argv):
            out = argv[i + 1]
            i += 2
        elif a == "--limit" and i + 1 < len(argv):
            limit = int(argv[i + 1])
            i += 2
        elif not a.startswith("--") and not out:
            out = a
            i += 1
        else:
            print("unknown arg: {}".format(a), file=sys.stderr)
            return 1
    if not out:
        here = os.path.dirname(os.path.abspath(__file__))
        out = os.path.join(here, "genuine_sessions.txt")
    db_path = find_db(db)
    if not os.path.exists(db_path):
        print("opencode.db not found: {}".format(db_path), file=sys.stderr)
        return 1
    try:
        ids = harvest(db_path, limit)
    except Exception as e:
        print("harvest failed: {!r}".format(e), file=sys.stderr)
        return 1
    if not ids:
        print("no genuine sessions found in {}".format(db_path), file=sys.stderr)
        return 1
    tmp = out + ".tmp"
    with open(tmp, "w", encoding="utf-8", newline="\n") as f:
        for sid in ids:
            f.write(sid + "\n")
    os.replace(tmp, out)
    print("harvested {} genuine sessions -> {}".format(len(ids), out))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
