#!/usr/bin/env python3
"""보낸 것은 도착이 아니다: 소유자 동작과 조정 세션이 수신함(/api/inbox)을 읽은 때를 맞대어, 오래 안 읽힌 동작을 센다.

세션 알림(session_inbox)의 sent 는 소켓에 썼다는 뜻일 뿐이다. 받는 세션이 대화상자에 붙들었거나, 끝났거나, 알림을 받고도
수신함을 안 읽었는지는 보드가 모른다. 그래서 보드는 두 가지를 적는다.
  - 소유자 동작(바깥 포트의 저장된 쓰기): 프로젝트 · 카드 · 동작 · 때
  - 수신함 읽기(로컬 포트의 GET /api/inbox): 프로젝트 · 때
읽은 때보다 늦은 동작이 N 분(BOARD_UNREAD_MINUTES, 기본 15) 넘게 남으면 보드 화면이 경고한다.

순수 규칙(record_action · record_read · unread_of)과 파일 보관(Watch)을 나눈다. 파일은 보드 설정 폴더의 0600 JSON 하나
(보드를 다시 띄워도 경고가 사라지지 않게)이고, 깨졌으면 빈 것으로 시작한다(경고는 덤이라 보드를 막지 않는다).
"""
import json
import os
import sys
import threading
import time

from session_inbox import write_private  # 0600 로 새로 쓰고 바꿔 끼우기(등록 파일과 같은 한 곳)

KEEP = 100  # 프로젝트마다 기억하는 안 읽힌 동작 수(넘으면 오래된 것부터 버린다, 수는 dropped 로 센다)


def _project(state, project):
    p = state.setdefault("projects", {}).setdefault(project, {})
    p.setdefault("pending", [])
    return p


def record_action(state, project, item, action, now):
    """소유자 동작 하나를 안 읽힘으로 적는다."""
    p = _project(state, project)
    p["pending"].append({"item": item, "action": action, "at": now})
    if len(p["pending"]) > KEEP:
        gone = p["pending"][:-KEEP]
        p["dropped"] = p.get("dropped", 0) + len(gone)
        p["dropped_at"] = min([p.get("dropped_at", gone[0]["at"])] + [a["at"] for a in gone])
        p["pending"] = p["pending"][-KEEP:]
    return state


def record_read(state, project, now):
    """수신함을 읽었다: 그때까지의 동작은 읽힘."""
    p = _project(state, project)
    p["read_at"] = now
    p["pending"] = [a for a in p["pending"] if a["at"] > now]
    if p.get("dropped_at", now + 1) <= now:  # 버린 것도 그때까지의 동작이면 읽힘
        p.pop("dropped", None)
        p.pop("dropped_at", None)
    return state


def unread_of(state, project, now, minutes):
    """화면이 보는 것: 안 읽힌 동작 수, 그중 minutes 분 넘은 것(late), 가장 오래된 때, 마지막 읽은 때."""
    p = (state.get("projects") or {}).get(project) or {}
    pending = p.get("pending") or []
    late = [a for a in pending if now - a["at"] > minutes * 60]
    dropped, dropped_at = p.get("dropped", 0), p.get("dropped_at")
    late_dropped = dropped if dropped_at is not None and now - dropped_at > minutes * 60 else 0
    times = [a["at"] for a in pending] + ([dropped_at] if dropped and dropped_at is not None else [])
    return {"count": len(pending) + dropped, "late": len(late) + late_dropped,
            "oldest_at": min(times, default=None), "read_at": p.get("read_at"),
            "minutes": minutes, "late_items": sorted({a["item"] for a in late if a.get("item")})[:10]}


class Watch:
    """파일 하나에 보관하는 상태. 보드는 여러 실로 요청을 받으므로 잠금 하나로 읽고 쓴다."""

    def __init__(self, path, minutes, clock=time.time):
        self.path, self.minutes, self.clock = path, minutes, clock
        self.lock = threading.Lock()
        self.state = self._load()

    def _load(self):
        """보관 파일을 읽되 꼴을 다시 짓는다(손으로 고쳤거나 깨진 파일이 화면 응답을 터뜨리지 않게)."""
        try:
            with open(self.path) as f:
                projects = json.load(f).get("projects")
        except FileNotFoundError:
            projects = None
        except (OSError, ValueError, AttributeError) as e:  # 깨진 파일은 빈 것으로 시작하고 다음 저장이 덮는다: 그 사실을 남긴다
            sys.stderr.write("BOARD_INBOX_WATCH_RESET %s\n" % e.__class__.__name__)
            projects = None
        clean = {}
        for name, p in (projects or {}).items() if isinstance(projects, dict) else ():
            if not isinstance(p, dict):
                continue
            num = (int, float)
            pending = p.get("pending") if isinstance(p.get("pending"), list) else []
            c = {"pending": [{"item": str(a.get("item") or ""), "action": str(a.get("action") or ""), "at": a["at"]}
                             for a in pending if isinstance(a, dict) and isinstance(a.get("at"), num)
                             and not isinstance(a.get("at"), bool) and 0 <= a["at"] < 1e11]}
            for key, kind in (("read_at", num), ("dropped", int), ("dropped_at", num)):
                if isinstance(p.get(key), kind) and not isinstance(p.get(key), bool) and 0 <= p[key] < 1e11:
                    c[key] = p[key]
            if "dropped" in c and "dropped_at" not in c:
                c["dropped_at"] = 0
            clean[str(name)] = c
        return {"projects": clean}

    def _save(self):
        try:
            os.makedirs(os.path.dirname(self.path), mode=0o700, exist_ok=True)
            write_private(self.path, self.state)
        except OSError as e:  # 보관을 못 해도 이번 실행 동안은 기억한다
            sys.stderr.write("BOARD_INBOX_WATCH_SAVE_FAILED %s\n" % e.__class__.__name__)

    def acted(self, project, item, action):
        with self.lock:
            record_action(self.state, project, item, action, self.clock())
            self._save()

    def read(self, project, at=None):
        """at: 읽기를 시작한 때(그 뒤에 저장된 동작은 그 읽기에 없었다). 없으면 지금."""
        with self.lock:
            record_read(self.state, project, self.clock() if at is None else at)
            self._save()

    def unread(self, project):
        with self.lock:
            return unread_of(self.state, project, self.clock(), self.minutes)
