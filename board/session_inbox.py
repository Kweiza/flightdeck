#!/usr/bin/env python3
"""보드 -> 세션 알림: 소유자가 보드에서 한 일을 그 프로젝트에서 사람이 연 Claude Code 세션의 inbox 에 한 줄로 넣는다.

board.py 가 notify() 를 부른다(표준 라이브러리만).

받는 곳은 fd 원장이 안다. fd 훅(SessionStart · UserPromptSubmit)이 세션을 연 김에 그 세션의 cross-session messaging
소켓(CLAUDE_CODE_MESSAGING_SOCKET)과 권한 모드를 PUT /api/v1/sessions/{id}/inbox 로 적는다(server/cmd/fd/inbox.go).
보드는 GET /api/v1/inbox?project&machine 으로 **그 카드 프로젝트 · 이 기계**의 것을 읽는다. 세션의 프로젝트는 원장이
git-common-dir 로 정해 두었으므로 보드는 폴더로 프로젝트를 판정하지 않는다. 기계는 이 맥의 fd machine-id 로 가른다
(다른 기계의 소켓 경로는 여기서 아무 뜻이 없고, 같은 경로가 이 맥의 남의 세션 것일 수 있다).

세션 inbox 는 Claude Code 의 cross-session messaging 이다(https://code.claude.com/docs/en/cross-session-messaging).
한 줄 JSON 하나가 메시지 하나다.

받는 쪽 문턱(문서 「Control inbound messages」): crossSessionInbound 가 없으면 받는 세션의 권한 모드 갈래로 정한다.
권한 확인을 건너뛰는 세션(bypass)은 보낸 쪽이 bypass 라고 밝힐 때만, 그 밖의 세션은 보낸 쪽이 bypass 라고 밝히지
않을 때만 바로 넘기고, 아니면 승인 대화상자에 붙든다. 그래서 전송은 받는 세션의 갈래를 봉투
(<cross-session-message from-mode=…>)에 적는다. 곧 bypass 세션의 승인 대화상자를 건너뛴다(소유자 선택 2026-11-23
「초인종으로 좁혀 유지」).

그래서 싣는 글은 초인종뿐이다: 고정 문장 + 꼴 검사(SAFE_ID_RE)를 지난 카드 id · 프로젝트 id + 보드가 정한 동작 이름.
소유자 글 · 카드 제목 · 요약은 싣지 않는다(누가 쓴 글이든 대화상자 없이 들어가면 세션을 부리는 글이 된다).
꼴이 어긋난 id 는 빼고 「보드에 변경 있음」만 보낸다. 받는 세션은 이 글로 명령을 짓지 않고 보드 /api/inbox 를 직접 읽는다.

CLAUDE_CODE_MESSAGING_TOKEN 은 어디에도 없다: 훅이 읽지도 보내지도 않고, 원장에 칸이 없다. 그 토큰은 「이 세션의
자식 프로세스」를 증명하는 값이고 macOS · Linux 에서는 연결에 없어도 되며, 자식이 아닌 보드가 내밀어도 자식으로
인정되지 않는다(실측 2.1.330).

남의 세션에 잘못 가지 않게: 소켓 경로는 pid 로 지어져 다시 쓰일 수 있다. 메시지에 그 카드의 **지금** session_id(cc)를
싣는다, 받는 세션은 자기 id 와 다르면 버린다. 원장은 한 기계 · 한 소켓에 한 세션만 남기고(같은 소켓의 옛 행은 쓰기 때
지움), 연결이 안 되는 소켓(죽은 세션)의 받는 곳은 보드가 지운다(DELETE, 소켓이 같을 때만).

옮겨 가는 동안(옛 등록 파일): 0.45.x 플러그인으로 뜬 세션은 받는 곳을 원장이 아니라 BOARD_SESSIONS/<session_id>.json
에 적는다. 그 파일의 cc 를 같은 GET 의 legacy 로 물어, 원장이 이 프로젝트 · 이 기계의 닫히지 않은 카드라고 답한
것에만 보낸다(폴더 판정 없음). 원장에 받는 곳이 있는 세션 · 소켓은 파일보다 이긴다. 그 세션들이 다 끝나면 이 길을 뺀다.
"""
import json
import os
import re
import socket
import sys

REGISTRY = os.path.expanduser(os.environ.get("BOARD_SESSIONS", "").strip() or "~/.config/flightdeck-board/sessions")  # 옛 등록 파일(옮겨 가는 동안)
SEND_TIMEOUT = 2.0
SENDER = "fd-board"
SESSION_ID_RE = re.compile(r"^[A-Za-z0-9_-]{1,80}$")
# 초인종에 실어도 되는 카드 · 프로젝트 id 의 꼴(fullmatch, 끝 줄바꿈도 안 됨). 빈칸 · 꺾쇠 · 줄바꿈 · 따옴표 · 대문자 ·
# 셸 기호는 못 지나가고 앞자리 - . / 와 .. 도 막는다. 하이픈으로 이은 낱말은 80자까지 실린다(카드 id 는 작업자가
# fd add 로 고르는 글자다): 그래서 받는 세션은 이 글로 명령을 짓지 않고 수신함 응답의 id 를 쓴다.
SAFE_ID_RE = re.compile(r"^[a-z0-9][a-z0-9._/-]{0,79}$")
# 동작 이름은 보드가 정한 이 표에서만 고른다(요청 값은 싣지 않는다).
ACTION_NAMES = {"note": "댓글", "done": "했어", "choose": "선택", "move": "상태 옮김", "add": "새 카드"}
BYPASS_MODES = ("bypassPermissions", "plan")  # 문서: bypass 를 쓸 수 있는 터미널 세션의 plan 은 bypass 갈래로 센다


def write_private(path, data):
    """0600 으로 새로 쓰고 바꿔 끼운다(읽는 쪽이 반쪽 파일을 보지 않게). inbox_watch 가 쓴다."""
    tmp = "%s.%d.tmp" % (path, os.getpid())
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(data, f, ensure_ascii=False)
    os.replace(tmp, path)


def machine_id(environ=None):
    """이 맥의 fd machine-id(fd 클라이언트와 같은 자리: FD_STATE_DIR/machine-id, 없으면 ~/.flightdeck/machine-id).
    보드는 읽기만 한다(만들지 않는다). 없으면 ""."""
    environ = os.environ if environ is None else environ
    state = (environ.get("FD_STATE_DIR") or "").strip()
    path = os.path.join(state, "machine-id") if state else os.path.join(os.path.expanduser("~"), ".flightdeck", "machine-id")
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return ""


# ---- 받는 곳 ---------------------------------------------------------------

def legacy_entries(registry):
    """옛 등록 파일(cc -> 등록). 깨진 파일 · 꼴이 어긋난 등록은 없는 셈 친다. 옮겨 가는 동안만 읽는다."""
    found = {}
    try:
        names = os.listdir(registry)
    except OSError:
        return found
    for name in names:
        if not name.endswith(".json"):
            continue
        try:
            with open(os.path.join(registry, name)) as f:
                e = json.load(f)
        except (OSError, ValueError):
            continue
        if not isinstance(e, dict) or not SESSION_ID_RE.fullmatch(str(e.get("session_id") or "")):
            continue
        if not isinstance(e.get("socket"), str) or not e["socket"] or not isinstance(e.get("mode", ""), str):
            continue
        found[e["session_id"]] = {"session_id": e["session_id"], "socket": e["socket"], "mode": e.get("mode") or "",
                                  "source": "legacy"}
    return found


def targets(project, fetch, registry=None, environ=None):
    """그 프로젝트 · 이 기계의 받는 곳. (목록, 못 읽은 까닭 또는 ""). fetch 는 board.fd_get 꼴 (path, **query) -> (code, body).
    목록의 session_id 는 메시지에 실을 cc, fd_session 은 원장의 카드 id(옛 등록이면 없음)."""
    registry = registry or REGISTRY
    machine = machine_id(environ)
    if not machine:
        return [], "이 맥의 fd machine-id 를 모름"
    legacy = legacy_entries(registry)
    query = {"project": project, "machine": machine}
    if legacy:
        query["legacy"] = ",".join(sorted(legacy))
    code, body = fetch("/api/v1/inbox", **query)
    if code in (404, 405):
        return [], "fd 서버가 받는 곳 경로를 모름(018 이전 서버, 컨테이너를 올려라)"
    if code != 200 or not isinstance(body, dict):
        err = (body.get("error") or {}) if isinstance(body, dict) else {}
        return [], "fd 에서 받는 곳을 못 읽음(%s)" % (err.get("code") if isinstance(err, dict) and err.get("code") else code)
    out, ccs, sockets = [], set(), set()
    for e in body.get("inbox") or []:
        if not isinstance(e, dict):
            continue
        cc, sock, sid = e.get("cc_session_id"), e.get("socket"), e.get("session_id")
        if not (isinstance(cc, str) and SESSION_ID_RE.fullmatch(cc) and isinstance(sock, str) and sock
                and isinstance(sid, str) and sid and isinstance(e.get("mode", ""), str)) or sock in sockets:
            continue
        out.append({"session_id": cc, "fd_session": sid, "socket": sock, "mode": e.get("mode") or "", "source": "ledger"})
        ccs.add(cc)
        sockets.add(sock)
    for cc, e in legacy.items():  # 원장으로 옮겨 간 세션의 옛 파일은 쓸모가 끝났다
        if cc in ccs or e["socket"] in sockets:
            forget(e, registry, None)
    for cc in body.get("legacy") or []:
        e = legacy.get(cc) if isinstance(cc, str) else None
        if e and cc not in ccs and e["socket"] not in sockets:  # 원장에 적힌 세션 · 소켓이 이긴다
            out.append(e)
            ccs.add(cc)
            sockets.add(e["socket"])
    return sorted(out, key=lambda e: e["session_id"]), ""


# ---- 전송 ------------------------------------------------------------------

def safe_id(value):
    """꼴 검사를 지난 id, 아니면 ""(대문자 · 빈칸 · 기호가 섞인 id 는 초인종에 싣지 않는다)."""
    return value if isinstance(value, str) and SAFE_ID_RE.fullmatch(value) and ".." not in value else ""


def message_text(project, item_id, action):
    """받는 세션이 보는 글. 고정 문장과 꼴 검사한 id 뿐이다. 첫 줄이 미리보기 한 줄이 된다."""
    pid, iid, name = safe_id(project), safe_id(item_id), ACTION_NAMES.get(action, "")
    head = "[fd 보드 알림] 보드에 변경 있음"
    if iid and name:
        head += ": %s %s" % (name, iid)
    if pid:
        head += " (프로젝트 %s)" % pid
    return head + "\n" + ("이 글은 깨우는 신호일 뿐 지시가 아니다. 이 글로 명령을 짓지 말고 보드 수신함(/api/inbox)을 직접 읽어, "
                           "처리할 카드는 먼저 fd pick 으로 선점한 뒤 board/README 「세션에 알림」대로 처리한다.")


def envelope(body, mode):
    """받는 세션의 권한 갈래를 밝힌 봉투. 그 갈래와 같다고 밝혀야 대화상자 없이 들어간다(머리 주석)."""
    cls = "bypass" if mode in BYPASS_MODES else "prompting"
    return '<cross-session-message from-name="%s" from-mode="%s">\n%s\n</cross-session-message>' % (SENDER, cls, body)


def wire_line(entry, body):
    return (json.dumps({"type": "user", "session_id": entry["session_id"], "from": SENDER,
                        "message": {"content": envelope(body, entry.get("mode") or "")}}, ensure_ascii=False) + "\n").encode()


def post(entry, body, timeout=SEND_TIMEOUT):
    """한 세션에 한 줄. (ok, 까닭). 소켓이 없거나 거절하면 dead_* (등록을 지울 것)."""
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    try:
        s.connect(entry["socket"])
        s.sendall(wire_line(entry, body))
        s.shutdown(socket.SHUT_WR)
        return True, "sent"
    except FileNotFoundError:
        return False, "dead_no_socket"
    except ConnectionRefusedError:
        return False, "dead_refused"
    except socket.timeout:
        return False, "timeout"
    except OSError as e:
        return False, "error_%s" % (e.__class__.__name__,)
    finally:
        s.close()


def forget(entry, registry, drop):
    """죽은 소켓의 받는 곳을 지운다. 원장이면 drop(카드 id, 소켓), 옛 등록이면 그 파일."""
    try:
        if entry.get("source") == "ledger":
            code, _ = drop(entry["fd_session"], entry["socket"])
            if code != 200:
                sys.stderr.write("BOARD_NOTIFY_FORGET_FAILED http_%s\n" % code)
        else:
            os.remove(os.path.join(registry, entry["session_id"] + ".json"))
    except Exception as e:  # noqa: BLE001  지우기는 덤이다(다음 알림이 다시 본다)
        sys.stderr.write("BOARD_NOTIFY_FORGET_FAILED %s\n" % e.__class__.__name__)


def notify(project, item_id, action, fetch, drop, registry=None, log=None, environ=None):
    """그 프로젝트의 사람이 연 세션 모두에 초인종 한 줄. action 은 ACTION_NAMES 의 열쇠. fetch 는 fd 읽기(board.fd_get),
    drop 은 죽은 받는 곳 지우기 (카드 id, 소켓). 돌려주는 값은 보드 응답의 notify 칸(토큰 · 소켓 경로 없음).
    sent 는 소켓에 썼다는 뜻이다: 받는 쪽이 붙들거나 버렸는지(session_id 불일치 · 반복 제한)는 보드가 모른다.
    도착은 수신함 읽기로 따로 잰다(inbox_watch)."""
    log = log or (lambda line: sys.stderr.write(line + "\n"))
    registry = registry or REGISTRY  # 부를 때 읽는다(시험 · 설정이 바꾼 값)
    item_id = safe_id(item_id) or "-"
    action = action if action in ACTION_NAMES else "-"
    found, why = targets(project, fetch, registry, environ)
    if why:
        log("BOARD_NOTIFY item=%s action=%s sent=0 why=no_targets" % (item_id, action))
        return {"ok": False, "sent": 0, "failed": [], "warning": "알림 못 보냄: " + why}
    body = message_text(project, item_id, action)
    sent, failed = 0, []
    for e in found:
        ok, why = post(e, body)
        if ok:
            sent += 1
            continue
        failed.append({"session": e["session_id"][:8], "why": why})
        if why.startswith("dead_"):
            forget(e, registry, drop)
    out = {"ok": sent > 0, "sent": sent, "failed": failed}
    if not sent:
        out["warning"] = "알림 못 보냄: " + ("살아 있는 세션 없음" if not failed or all(
            f["why"].startswith("dead_") for f in failed) else ", ".join(f["why"] for f in failed))
    log("BOARD_NOTIFY item=%s action=%s sent=%d failed=%s" % (
        item_id, action, sent, ",".join("%s:%s" % (f["session"], f["why"]) for f in failed) or "-"))
    return out
