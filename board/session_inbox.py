#!/usr/bin/env python3
"""보드 -> 세션 알림: 소유자가 보드에서 한 일을 그 프로젝트에서 사람이 연 Claude Code 세션의 inbox 에 한 줄로 넣는다.

두 쪽이 이 파일 하나를 쓴다(표준 라이브러리만).
  등록(훅):  python3 -I session_inbox.py hook   < 훅 페이로드(JSON)
             플러그인 hooks.json 의 SessionStart · UserPromptSubmit 이 부른다(SessionEnd 는 /clear · resume 에만
             와서 쓰지 않는다: 끝난 세션은 전송 때 연결이 안 되는 것으로 알고 지운다).
  전송(보드): board.py 가 notify() 를 부른다.

세션 inbox 는 Claude Code 의 cross-session messaging 이다(https://code.claude.com/docs/en/cross-session-messaging).
훅 환경의 CLAUDE_CODE_MESSAGING_SOCKET 이 그 세션의 유닉스 소켓이고, 한 줄 JSON 하나가 메시지 하나다.

받는 쪽 문턱(문서 「Control inbound messages」): crossSessionInbound 가 없으면 받는 세션의 권한 모드 갈래로 정한다.
권한 확인을 건너뛰는 세션(bypass)은 보낸 쪽이 bypass 라고 밝힐 때만, 그 밖의 세션은 보낸 쪽이 bypass 라고 밝히지
않을 때만 바로 넘기고, 아니면 승인 대화상자에 붙든다. 그래서 등록은 그 세션의 권한 모드를 적고(UserPromptSubmit 마다
고침), 전송은 받는 세션의 갈래를 봉투(<cross-session-message from-mode=…>)에 적는다. 곧 bypass 세션의 승인 대화상자를
건너뛴다(소유자 선택 2026-11-23 「초인종으로 좁혀 유지」).

그래서 싣는 글은 초인종뿐이다: 고정 문장 + 꼴 검사(SAFE_ID_RE)를 지난 카드 id · 프로젝트 id + 보드가 정한 동작 이름.
소유자 글 · 카드 제목 · 요약은 싣지 않는다(누가 쓴 글이든 대화상자 없이 들어가면 세션을 부리는 글이 된다).
꼴이 어긋난 id 는 빼고 「보드에 변경 있음」만 보낸다. 받는 세션은 이 글로 명령을 짓지 않고 보드 /api/inbox 를 직접 읽는다.

CLAUDE_CODE_MESSAGING_TOKEN 은 쓰지도 적지도 않는다. 그 토큰은 「이 세션의 자식 프로세스」를 증명하는 값이고 macOS ·
Linux 에서는 연결에 없어도 되며, 자식이 아닌 보드가 내밀어도 자식으로 인정되지 않는다(실측 2.1.330). 적지 않으면 샐 것도 없다.

남의 세션에 잘못 가지 않게: 소켓 경로는 pid 로 지어져 다시 쓰일 수 있다. 메시지에 등록한 session_id 를 싣는다,
받는 세션은 자기 id 와 다르면 버린다. 연결이 안 되는 소켓(죽은 세션)의 등록은 지운다.
"""
import json
import os
import re
import socket
import subprocess
import sys
import time

REGISTRY = os.path.expanduser(os.environ.get("BOARD_SESSIONS", "").strip() or "~/.config/flightdeck-board/sessions")
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


# ---- 등록 (훅) -------------------------------------------------------------

def attended(environ):
    """사람이 연 세션인가. -p · SDK 작업자는 CLAUDE_CODE_SESSION_ATTENDED=0, 터미널 세션은 1(없으면 entrypoint cli)."""
    flag = environ.get("CLAUDE_CODE_SESSION_ATTENDED")
    if flag is not None:
        return flag == "1"
    return environ.get("CLAUDE_CODE_ENTRYPOINT") == "cli"


def mode_from_args(args):
    """claude 명령줄에서 권한 모드. 못 정하면 ""."""
    words = args.split()
    if "--dangerously-skip-permissions" in words:
        return "bypassPermissions"
    for i, w in enumerate(words):
        if w == "--permission-mode" and i + 1 < len(words):
            return words[i + 1]
        if w.startswith("--permission-mode="):
            return w.split("=", 1)[1]
    return ""


def mode_from_settings(project_dir, home):
    """설정 파일의 permissions.defaultMode, 이기는 순서(local > project > user). 없으면 "default"."""
    for path in (os.path.join(project_dir, ".claude", "settings.local.json"),
                 os.path.join(project_dir, ".claude", "settings.json"),
                 os.path.join(home, ".claude", "settings.json")):
        try:
            with open(path) as f:
                mode = (json.load(f).get("permissions") or {}).get("defaultMode")
        except (OSError, ValueError, AttributeError):
            continue
        if isinstance(mode, str) and mode:
            return mode
    return "default"


def startup_mode(environ):
    """SessionStart 페이로드에는 permission_mode 가 없다: 명령줄 다음 설정 파일로 짐작하고, 첫 프롬프트가 바로잡는다."""
    pid = environ.get("CLAUDE_PID", "")
    if pid.isdigit():
        try:
            args = subprocess.run(["ps", "-o", "args=", "-p", pid], capture_output=True, text=True, timeout=2).stdout
        except (OSError, subprocess.SubprocessError):
            args = ""
        mode = mode_from_args(args)
        if mode:
            return mode
    project = environ.get("CLAUDE_PROJECT_DIR") or os.getcwd()
    return mode_from_settings(project, os.path.expanduser("~"))


def entry_path(registry, sid):
    return os.path.join(registry, sid + ".json")


def write_private(path, data):
    """0600 으로 새로 쓰고 바꿔 끼운다(읽는 쪽이 반쪽 파일을 보지 않게)."""
    tmp = "%s.%d.tmp" % (path, os.getpid())
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(data, f, ensure_ascii=False)
    os.replace(tmp, path)


def register(payload, environ, registry=None, now=None):
    """훅 한 번. 돌려주는 값은 시험용 한 낱말: registered · updated · skipped:<까닭>."""
    registry = registry or REGISTRY
    sid = payload.get("session_id") or environ.get("CLAUDE_CODE_SESSION_ID") or ""
    if not SESSION_ID_RE.fullmatch(sid):
        return "skipped:no_session_id"
    path = entry_path(registry, sid)
    if not os.path.isdir(os.path.dirname(registry)):
        return "skipped:no_board"  # 보드를 깔지 않은 기계(설정 폴더 없음)에는 아무것도 남기지 않는다
    if not attended(environ):
        return "skipped:unattended"
    sock = environ.get("CLAUDE_CODE_MESSAGING_SOCKET") or ""
    if not sock:
        return "skipped:no_socket"
    now = now or time.time()
    try:
        with open(path) as f:
            entry = json.load(f)
    except (OSError, ValueError):
        entry = {}
    mode = payload.get("permission_mode") or ""
    entry.update({"session_id": sid, "socket": sock, "cwd": payload.get("cwd") or entry.get("cwd") or "",
                  "project_dir": environ.get("CLAUDE_PROJECT_DIR") or entry.get("project_dir") or "",
                  "seen_at": now})  # 사람이 볼 때 쓰는 기록(판정에는 안 씀)
    if mode:
        entry["mode"] = mode
    elif not entry.get("mode"):
        entry["mode"] = startup_mode(environ)
    entry.setdefault("registered_at", now)
    os.makedirs(registry, mode=0o700, exist_ok=True)
    existed = os.path.exists(path)
    write_private(path, entry)
    for other in sessions(registry):  # 한 소켓은 한 살아 있는 세션의 것: 같은 소켓의 옛 id(/clear 전 · pid 재사용)는 낡았다
        if other["socket"] == sock and other["session_id"] != sid:
            try:
                os.remove(entry_path(registry, other["session_id"]))
            except OSError:
                pass
    return "updated" if existed else "registered"


# ---- 전송 (보드) ------------------------------------------------------------

def sessions(registry=REGISTRY):
    """등록된 세션 목록(깨진 파일은 건너뜀)."""
    found = []
    try:
        names = os.listdir(registry)
    except OSError:
        return []
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
        if not all(isinstance(e.get(k, ""), str) for k in ("socket", "cwd", "project_dir", "mode")) or not e.get("socket"):
            continue  # 꼴이 깨진 등록은 없는 셈 친다(알림이 저장된 동작의 응답을 깨지 않게)
        found.append(e)
    return sorted(found, key=lambda e: e["session_id"])


def under(path, root):
    if not path or not root:
        return False
    p, r = os.path.realpath(path), os.path.realpath(root)
    return p == r or p.startswith(r.rstrip("/") + "/")


def home_of(cwd, project_paths):
    """세션 폴더가 속한 fd 프로젝트 폴더: 감싸는 것 중 가장 깊은 하나(~/a 와 ~/a/repos/b 가 둘 다 프로젝트일 때 b)."""
    inside = [p for p in project_paths if under(cwd, p)]
    return max(inside, key=lambda p: len(os.path.realpath(p))) if inside else ""


def targets(project_path, project_paths, registry=REGISTRY):
    """그 프로젝트 폴더(그 안의 워크트리 포함)가 가장 깊은 집인 세션. project_paths 는 fd 의 모든 프로젝트 폴더."""
    want = os.path.realpath(project_path)
    paths = set(project_paths) | {project_path}
    return [e for e in sessions(registry)
            if os.path.realpath(home_of(e.get("cwd") or e.get("project_dir"), paths) or "/nonexistent") == want]


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


def notify(project, project_path, project_paths, item_id, action, registry=REGISTRY, log=None):
    """그 프로젝트의 사람이 연 세션 모두에 초인종 한 줄. action 은 ACTION_NAMES 의 열쇠. 돌려주는 값은 보드 응답의
    notify 칸(토큰 · 소켓 경로 없음). sent 는 소켓에 썼다는 뜻이다: 받는 쪽이 붙들거나 버렸는지(session_id 불일치 ·
    반복 제한)는 보드가 모른다. 도착은 수신함 읽기로 따로 잰다(inbox_watch)."""
    log = log or (lambda line: sys.stderr.write(line + "\n"))
    item_id = safe_id(item_id) or "-"
    action = action if action in ACTION_NAMES else "-"
    if not project_path:
        out = {"ok": False, "sent": 0, "failed": [], "warning": "알림 못 보냄: 프로젝트 %s 의 폴더를 모름" % (safe_id(project) or "?")}
        log("BOARD_NOTIFY item=%s action=%s sent=0 why=no_project_path" % (item_id, action))
        return out
    body = message_text(project, item_id, action)
    sent, failed = 0, []
    for e in targets(project_path, project_paths, registry):
        ok, why = post(e, body)
        if ok:
            sent += 1
            continue
        failed.append({"session": e["session_id"][:8], "why": why})
        if why.startswith("dead_"):
            try:
                os.remove(entry_path(registry, e["session_id"]))
            except OSError:
                pass
    out = {"ok": sent > 0, "sent": sent, "failed": failed}
    if not sent:
        out["warning"] = "알림 못 보냄: " + ("살아 있는 세션 없음" if not failed or all(
            f["why"].startswith("dead_") for f in failed) else ", ".join(f["why"] for f in failed))
    log("BOARD_NOTIFY item=%s action=%s sent=%d failed=%s" % (
        item_id, action, sent, ",".join("%s:%s" % (f["session"], f["why"]) for f in failed) or "-"))
    return out


def main(argv):
    if argv[1:] != ["hook"]:
        sys.stderr.write("쓰는 법: session_inbox.py hook < 훅 페이로드\n")
        return 2
    try:
        payload = json.load(sys.stdin)
    except ValueError:
        payload = {}
    try:
        register(payload if isinstance(payload, dict) else {}, os.environ)
    except OSError as e:
        sys.stderr.write("BOARD_SESSION_REGISTER_FAILED %s\n" % e.__class__.__name__)
    return 0  # 훅은 세션을 막지 않는다


if __name__ == "__main__":
    sys.exit(main(sys.argv))
