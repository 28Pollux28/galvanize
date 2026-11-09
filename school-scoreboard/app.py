import os
import re
import time
import unicodedata
from datetime import datetime, timezone

import pymysql
from flask import Flask, jsonify, render_template

app = Flask(__name__)

CACHE_TTL = int(os.environ.get("CACHE_TTL", "10"))
_cache = {"ts": 0.0, "payload": None}


def fold(value: str) -> str:
    text = unicodedata.normalize("NFKD", value or "")
    text = "".join(ch for ch in text if not unicodedata.combining(ch))
    text = text.lower().replace("&", " ").replace("'", " ").replace("’", " ")
    return " ".join(re.sub(r"[^a-z0-9]+", " ", text).split())


def canonical_school(affiliation: str) -> str | None:
    raw = (affiliation or "").strip()
    if not raw:
        return None
    key = fold(raw)
    if not key:
        return None

    if "jdis" in key or key in {"udes", "uds"} or "sherbrooke" in key:
        return "Université de Sherbrooke"
    if key in {"ulaval"} or "laval" in key:
        return "Université Laval"
    if key in {"ets", "dci"} or "ets" in key or "dddccc" in key:
        return "ÉTS"
    if "concordia" in key:
        return "Concordia University"
    if "poly" in key:
        return "Polytechnique Montréal"
    if key == "uqam":
        return "UQAM"
    if key == "udem":
        return "Université de Montréal"
    if "foy" in key:
        return "Cégep de Sainte-Foy"
    if "csi" in key and "jean" in key:
        return "CSI Saint-Jean"
    if "fish" in key and "fric" in key:
        return "Fish & Fric"
    return raw


def db_conn():
    return pymysql.connect(
        host=os.environ.get("MYSQL_HOST", "db"),
        user=os.environ.get("MYSQL_USER", "ctfd"),
        password=os.environ["MYSQL_PASSWORD"],
        database=os.environ.get("MYSQL_DATABASE", "ctfd"),
        charset="utf8mb4",
        cursorclass=pymysql.cursors.DictCursor,
        connect_timeout=5,
        read_timeout=10,
    )


def load_scoreboard():
    sql = """
        SELECT
            u.id AS user_id,
            u.name AS username,
            u.affiliation AS affiliation,
            COALESCE(SUM(c.value), 0) AS chall_score,
            COALESCE((
                SELECT SUM(a.value) FROM awards a WHERE a.user_id = u.id
            ), 0) AS award_score,
            COUNT(c.id) AS solves,
            MAX(s.date) AS last_solve
        FROM users u
        LEFT JOIN submissions s
            ON s.user_id = u.id AND s.type = 'correct'
        LEFT JOIN challenges c
            ON c.id = s.challenge_id AND c.state = 'visible'
        WHERE u.hidden = 0
          AND u.banned = 0
          AND u.type = 'user'
        GROUP BY u.id, u.name, u.affiliation
    """
    with db_conn() as conn:
        with conn.cursor() as cur:
            cur.execute(sql)
            rows = cur.fetchall()

    schools: dict[str, dict] = {}
    unaffiliated = 0
    for row in rows:
        score = int(row["chall_score"] or 0) + int(row["award_score"] or 0)
        school = canonical_school(row["affiliation"])
        if school is None:
            unaffiliated += 1
            continue
        entry = schools.setdefault(
            school,
            {
                "school": school,
                "players": [],
                "affiliations": set(),
            },
        )
        last = row["last_solve"]
        last_iso = last.isoformat(sep=" ") if last else None
        if row["affiliation"]:
            entry["affiliations"].add(row["affiliation"].strip())
        entry["players"].append(
            {
                "name": row["username"],
                "score": score,
                "solves": int(row["solves"] or 0),
                "affiliation": (row["affiliation"] or "").strip(),
                "last_solve": last_iso,
            }
        )

    ranked = []
    for entry in schools.values():
        entry["players"].sort(
            key=lambda p: (-p["score"], p["last_solve"] or "9999", p["name"].lower())
        )
        for i, player in enumerate(entry["players"], start=1):
            player["rank"] = i
        entry["player_count"] = len(entry["players"])
        entry["affiliations"] = sorted(entry["affiliations"], key=str.lower)
        ranked.append(entry)

    ranked.sort(key=lambda s: (-s["player_count"], s["school"].lower()))

    return {
        "generated_at": datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC"),
        "schools": ranked,
        "unaffiliated_players": unaffiliated,
        "school_count": len(ranked),
    }


def scoreboard():
    now = time.time()
    if _cache["payload"] is None or now - _cache["ts"] > CACHE_TTL:
        _cache["payload"] = load_scoreboard()
        _cache["ts"] = now
    return _cache["payload"]


@app.get("/")
def index():
    return render_template("index.html")


@app.get("/api/scoreboard")
def api_scoreboard():
    return jsonify(scoreboard())


@app.get("/health")
def health():
    return {"ok": True}
