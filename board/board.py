#!/usr/bin/env python3
"""flightdeck 칸반 보드: 소유자의 작업 데스크톱. 정적 페이지 하나, fd API 프록시, 로컬 파일 창구.

토큰은 이 프로세스만 안다(~/.flightdeck/config.json, 요청마다 다시 읽음). 페이지 · 응답 ·
로그에는 토큰을 싣지 않는다. 원본은 fd 하나다: 소유자가 보드에서 한 일은 fd 의 꼬리표와
판단 노트(note)로만 남고, 이 프로세스는 아무 상태도 갖지 않는다.

열(column) 판정은 column_of() 한 곳에 있다:
  끝남       = state done · dropped
  진행 중    = 꼬리표 status:running, 또는 어느 세션이 선점 중
  소유자 차례 = 꼬리표 status:owner(소유자가 할 일), status:owner-done · status:owner-ask(조정자에게 돌아간 것)
  대기       = 그 밖의 열린 항목

카드 본문 규약(README 「카드 본문 규약」)은 directives_of() 한 곳에서 읽는다.
결정 카드 꼴(### 질문 · 배경 · 보기: · 추천)은 decision_of(), 소유자가 고른 보기는 chosen_of() 에서 읽는다.
"""
import hmac
import ipaddress
import json
import mimetypes
import os
import re
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import inbox_watch  # noqa: E402
import session_inbox  # noqa: E402  (같은 폴더의 모듈, -I 는 스크립트 폴더를 path 에 넣지 않는다)


def env(name, default=""):
    return os.environ.get(name, "").strip() or default


# 설정은 모두 환경 변수다(README 「설정」). 기계마다 다른 값은 install.sh 가 launchd plist 에 적는다.
CONFIG = os.path.expanduser(env("FD_CONFIG", "~/.flightdeck/config.json"))
LINKS = os.path.expanduser(env("BOARD_LINKS", "~/.config/flightdeck-board/links.json"))
ADDR = env("BOARD_ADDR", "127.0.0.1")
PORT = int(env("BOARD_PORT", "7430"))  # tailscale serve 가 가리키는 곳: 모든 요청이 소유자 신원을 지녀야 한다
LOCAL_PORT = int(env("BOARD_LOCAL_PORT", "7431"))  # 에이전트 · 로컬 확인용, serve 가 가리키지 않는다(127.0.0.1 고정)
DEFAULT_PROJECT = env("BOARD_PROJECT")  # 비면 화면이 프로젝트 목록의 첫째를 고른다
TIMEOUT = 10
MAX_POST = 64 * 1024
CHUNK = 256 * 1024
# 하네스 스크래치패드(/private/tmp/claude-<uid>)와 저장소들. 작업자가 카드에 다는 그림 · 파일이 사는 곳이다.
DEFAULT_FILE_ROOTS = "~/Developments:/private/tmp/claude-%d" % os.getuid()


def roots_of(spec):
    """'a:b' -> (FILE_ROOTS, ROOT_PREFIXES). 허용 판정은 realpath(FILE_ROOTS), 조각 검사는 적힌 꼴 · realpath ·
    /private 를 뗀 꼴(/tmp → /private/tmp 같은 macOS 별칭) 어느 쪽으로 와도 뿌리를 떼도록 ROOT_PREFIXES 로."""
    given = [os.path.normpath(os.path.expanduser(p.strip())) for p in spec.split(":") if p.strip()]
    roots = [os.path.realpath(p) for p in given]
    prefixes = set(given) | set(roots)
    prefixes |= {p[len("/private"):] for p in prefixes if p.startswith("/private/")}
    return roots, sorted(prefixes, key=len, reverse=True)


FILE_ROOTS, ROOT_PREFIXES = roots_of(env("BOARD_FILE_ROOTS", DEFAULT_FILE_ROOTS))

# 미리보기 확인(/api/embed)이 대신 부를 수 있는 곳: 이 기계 tailnet 접미사 아래 이름, 그리고 정한 포트의 이 맥.
TAILNET = env("BOARD_TAILNET").lower().strip(".")  # 예: <tailnet>.ts.net (install.sh 가 tailscale 에서 읽음)
PREVIEW_PORTS = {int(p) for p in env("BOARD_PREVIEW_PORTS").split(",") if p.strip().isdigit()}

# 접근 비밀 · 소유자 로그인: 저장소 밖 파일(install.sh 가 0600 으로 만든다). 값은 응답 · 로그에 싣지 않는다.
AUTH_FILE = os.path.expanduser(env("BOARD_AUTH", "~/.config/flightdeck-board/auth.json"))
# 보낸 것은 도착이 아니다(README 「세션에 알림」): 소유자 동작과 수신함 읽기를 맞대어 오래 안 읽힌 것을 화면에 경고.
WATCH_FILE = os.path.expanduser(env("BOARD_WATCH", "~/.config/flightdeck-board/inbox-watch.json"))
UNREAD_MINUTES = max(1, int(env("BOARD_UNREAD_MINUTES", "15") or 15))
WATCH = inbox_watch.Watch(WATCH_FILE, UNREAD_MINUTES)
COOKIE = "fdboard"


def read_auth(path):
    """{"secret": str, "owner_logins": [str]} 또는 None(없거나 깨짐)."""
    try:
        with open(path) as f:
            a = json.load(f)
    except (OSError, ValueError):
        return None
    if not isinstance(a, dict) or not isinstance(a.get("secret"), str) or len(a["secret"]) < 32:
        return None
    logins = [l.strip().lower() for l in a.get("owner_logins") or [] if isinstance(l, str) and l.strip()]
    return {"secret": a["secret"], "owner_logins": logins}


AUTH = read_auth(AUTH_FILE)

# 꼬리표: 열을 가르는 것. 카드의 꼬리표 칩에서는 뺀다(열이 이미 말한다).
OWNER_TODO, OWNER_DONE, OWNER_ASK, RUNNING = "status:owner", "status:owner-done", "status:owner-ask", "status:running"
STATUS_LABELS = (OWNER_TODO, OWNER_DONE, OWNER_ASK, RUNNING)
# 보드가 남기는 판단 노트의 제목 머리. 검색 낱말이기도 하다(/api/inbox 가 이 낱말로 찾는다).
NOTE_MARK = "[fdboard]"
NOTE_KIND = "decision"
CHOICE_PREFIX = "소유자 선택: "
MOVE_PREFIX = "소유자: 옮김 -> "  # 상태 옮기기가 남기는 노트 머리(제목 · 본문)
COLUMN_NAMES = {"waiting": "대기", "owner": "소유자 차례", "running": "진행 중"}  # 선택 버튼이 남기는 노트 본문 머리(chosen_of 가 다시 읽는다)

DIRECTIVE_RE = re.compile(r"^\s*(미리보기|그림|영상|파일|링크|커밋|소유자|선택)\s*:\s*(.+?)\s*$", re.M)
SHA_RE = re.compile(r"\b[0-9a-f]{7,40}\b")


# ---- 순수 규칙 -------------------------------------------------------------

def column_of(item, claimed):
    if item.get("State") in ("done", "dropped"):
        return "done"
    labels = item.get("Labels") or []
    if RUNNING in labels or item["ID"] in claimed:
        return "running"
    if OWNER_TODO in labels or OWNER_DONE in labels or OWNER_ASK in labels:
        return "owner"
    return "waiting"


def owner_state_of(labels):
    """소유자 열 안의 묶음: todo(소유자가 할 일) · back(조정자에게 돌아간 것) · ''."""
    labels = labels or []
    if OWNER_DONE in labels or OWNER_ASK in labels:
        return "back"
    if OWNER_TODO in labels:
        return "todo"
    return ""


def github_url(remote):
    """git remote 주소 -> https://github.com/<org>/<repo>, 깃허브가 아니면 ''."""
    remote = (remote or "").strip()
    m = re.match(r"^(?:git@github\.com:|ssh://git@github\.com/|https://github\.com/)([^/]+/[^/]+?)(?:\.git)?/?$", remote)
    return "https://github.com/" + m.group(1) if m else ""


def directives_of(body, repo_url=""):
    """본문의 규약 줄을 모은다. 한 종류 여러 줄 가능, 모르는 접두어는 본문 그대로."""
    d = {"previews": [], "images": [], "videos": [], "files": [], "links": [], "commits": [],
         "owner": [], "choices": []}
    for key, val in DIRECTIVE_RE.findall(body or ""):
        if key == "미리보기" and val.startswith(("https://", "http://")):
            d["previews"].append(val)
        elif key == "그림":
            d["images"].append(val)
        elif key == "영상":
            d["videos"].append(val)
        elif key == "파일":
            d["files"].append(val)
        elif key == "링크":
            name, _, url = val.rpartition("|")
            url = url.strip()
            if url.startswith(("https://", "http://")):
                d["links"].append({"name": name.strip() or url, "url": url})
        elif key == "커밋":
            for sha in SHA_RE.findall(val):
                d["commits"].append({"sha": sha, "url": repo_url + "/commit/" + sha if repo_url else ""})
        elif key == "소유자":
            d["owner"].append(val)
        elif key == "선택":
            d["choices"] += [c.strip() for c in val.split("|") if c.strip()]
    return d


# 결정 카드 본문 꼴(README 「소유자에게 고르게 할 때」): ### 질문 / ### 배경 / ### 보기: 이름 / ### 추천
DECISION_HEAD_RE = re.compile(r"^###\s+(질문|배경|추천|보기\s*[:：]\s*(.+?))\s*$")
RECOMMEND_MARK = "(추천)"
JUDGE_RE = re.compile(r"^\s*[-*]?\s*(Claude|Codex|Fable|Opus|astra)[^:：]{0,30}[:：]\s*(.+?)\s*$", re.I)
SPLIT_WORDS = ("의견 갈림", "의견이 갈", "갈렸")


def bare_choice(name):
    """보기 이름에서 끝의 「(추천)」을 뗀 것. 본문 머리와 선택: 줄을 이걸로 맞댄다."""
    name = (name or "").strip()
    if name.endswith(RECOMMEND_MARK):
        name = name[:-len(RECOMMEND_MARK)].rstrip()
    return name


def decision_sections(body):
    """본문을 결정 꼴 머리로 나눈다 -> [(머리, 보기 이름 또는 None, 내용 줄)]. 규약 줄은 내용에서 뺀다."""
    out, cur = [], None
    for line in (body or "").splitlines():
        m = DECISION_HEAD_RE.match(line.strip())
        if m:
            kind = "보기" if m.group(2) is not None else m.group(1)
            cur = (kind, m.group(2), [])
            out.append(cur)
        elif line.strip().startswith("## ") or line.strip().startswith("### "):
            cur = None  # 꼴 밖의 머리가 나오면 그 절은 끝난다
        elif cur is not None and not DIRECTIVE_RE.match(line):
            cur[2].append(line)
    return [(k, n, "\n".join(ls).strip()) for k, n, ls in out]


def judges_of(text, names):
    """추천 절의 판정자 줄(Claude: … / Codex: …)과 그 줄이 처음 이름 댄 보기. 갈림 여부를 함께 낸다."""
    judges, rest = [], []
    for line in text.splitlines():
        m = JUDGE_RE.match(line)
        if not m:
            rest.append(line)
            continue
        said = m.group(2)
        hits = [(said.find(n), n) for n in names if n and n in said]
        judges.append({"who": m.group(1), "text": said, "pick": min(hits)[1] if hits else ""})
    picks = {j["pick"] for j in judges if j["pick"]}
    split = len(picks) > 1 or any(w in text for w in SPLIT_WORDS)
    return judges, split, "\n".join(rest).strip()


def decision_of(body, choices):
    """결정 꼴이 있으면 {question, background, options, recommendation, judges, split, recommended}, 없으면 None.

    보기 이름은 선택: 줄의 보기와 「(추천)」을 뗀 채로 맞대고, 버튼이 보낼 값(choice)은 선택: 줄의 원문이다.
    """
    secs = decision_sections(body)
    question = next((t for k, _, t in secs if k == "질문"), None)
    opts = [(n, t) for k, n, t in secs if k == "보기"]
    if question is None or not opts:
        return None
    by_bare = {bare_choice(c): c for c in choices}
    options = []
    for name, text in opts:
        raw = by_bare.get(bare_choice(name), "")
        options.append({"name": bare_choice(name), "body": text, "choice": raw,
                        "recommended": name.strip().endswith(RECOMMEND_MARK) or raw.endswith(RECOMMEND_MARK)})
    rec_text = next((t for k, _, t in secs if k == "추천"), "")
    judges, split, rec_rest = judges_of(rec_text, [o["name"] for o in options])
    rec = next((o["name"] for o in options if o["recommended"]), "")
    return {"question": question, "background": next((t for k, _, t in secs if k == "배경"), ""),
            "options": options, "recommendation": rec_rest, "judges": judges, "split": split,
            "recommended": rec, "chosen": ""}


def chosen_of(judgments, labels):
    """소유자가 보드에서 고른 보기(가장 늦은 「소유자 선택:」 노트). 조정자가 다시 묻는 중(status:owner)이면 ''."""
    if OWNER_TODO in (labels or []):
        return ""
    picks = sorted((j for j in judgments or [] if is_owner_note(j)
                    and (j.get("Body") or "").startswith(CHOICE_PREFIX)), key=lambda j: j.get("At") or "")
    return picks[-1]["Body"][len(CHOICE_PREFIX):].strip() if picks else ""


def summary_of(body):
    for line in (body or "").splitlines():
        line = line.strip()
        if line and not DIRECTIVE_RE.match(line) and not line.startswith("(Artifact 장부"):
            return line[:200]
    return ""


def card_of(item, claimed, repo_url=""):
    body = item.get("Body") or ""
    d = directives_of(body, repo_url)
    decision = decision_of(body, d["choices"])
    labels = item.get("Labels") or []
    if not d["commits"] and item.get("LandedRef"):
        d["commits"] = [{"sha": item["LandedRef"],
                         "url": repo_url + "/commit/" + item["LandedRef"] if repo_url else ""}]
    return {
        "id": item["ID"],
        "title": item.get("Title") or item["ID"],
        "summary": (decision["question"].splitlines() or [""])[0][:200] if decision else summary_of(body),
        "decision": decision,
        "labels": [l for l in labels if l not in STATUS_LABELS],
        "status_labels": [l for l in labels if l in STATUS_LABELS],
        "owner_state": owner_state_of(labels),
        "state": item.get("State"),
        "close_reason": item.get("CloseReason") or "",
        "column": column_of(item, claimed),
        "claimed": item["ID"] in claimed,
        "created_at": item.get("CreatedAt"),
        "closed_at": item.get("ClosedAt"),
        "updated_at": item.get("ClosedAt") or item.get("CreatedAt"),
        **d,
    }


def claimed_ids(dash):
    ids = set()
    for s in dash.get("sessions") or []:
        for c in (s.get("view") or {}).get("Claims") or []:
            # 서버 판에 따라 선점이 항목 id 문자열이거나 {ItemID, ReleasedAt} 객체다
            if isinstance(c, str):
                ids.add(c)
            elif isinstance(c, dict) and c.get("ItemID") and not c.get("ReleasedAt"):
                ids.add(c["ItemID"])
    return ids


def add_claimed_items(dash, project):
    """새 flightdeck 은 선점된 항목을 open_items 에서 뺀다. 진행 중 열에 보이도록 하나씩 읽어 붙인다."""
    items = dash.setdefault("open_items", []) or []
    dash["open_items"] = items
    have = {i.get("ID") for i in items}
    for iid in sorted(claimed_ids(dash) - have):
        code, body = fd_get(item_path(iid), project=project)
        if code == 200 and isinstance(body.get("item"), dict):
            items.append(body["item"])


def board_of(project, dash, closed, repo_url=""):
    claimed = claimed_ids(dash)
    cards = [card_of(i, claimed, repo_url) for i in dash.get("open_items") or []]
    cards += [card_of(i, claimed, repo_url) for i in closed.get("items") or []]
    counts = {k: 0 for k in ("owner", "running", "waiting", "done")}
    for c in cards:
        counts[c["column"]] += 1
    return {"project": project, "at": dash.get("at"), "counts": counts, "repo": repo_url, "cards": cards}


def move_labels(target):
    """열 이름 -> (더할 꼬리표, 뺄 꼬리표). 끝남은 여기서 다루지 않는다(닫기는 조정자가 커밋과 함께)."""
    if target == "running":
        return [RUNNING], [OWNER_TODO, OWNER_DONE, OWNER_ASK]
    if target == "owner":
        return [OWNER_TODO], [RUNNING, OWNER_DONE, OWNER_ASK]
    if target == "waiting":
        return [], list(STATUS_LABELS)
    return None


def embed_verdict(headers):
    """미리보기 응답 머리 -> (보드 iframe 안에서 열리는가, 까닭).

    보드는 스레드 Godot 를 iframe 안에서 돌리려고 COEP 를 건다. 그러면 iframe 문서는
    COEP 와 함께 Cross-Origin-Resource-Policy(cross-origin · same-site)를 보내야 열린다(브라우저 실측)."""
    h = {k.lower(): (v or "").strip().lower() for k, v in headers.items()}
    if h.get("x-frame-options") in ("deny", "sameorigin"):
        return False, "X-Frame-Options=%s" % h["x-frame-options"]
    if "frame-ancestors" in h.get("content-security-policy", ""):
        return False, "CSP frame-ancestors"
    if h.get("cross-origin-embedder-policy", "unsafe-none") in ("", "unsafe-none"):
        return False, "Cross-Origin-Embedder-Policy 없음"
    if h.get("cross-origin-resource-policy") not in ("cross-origin", "same-site"):
        return False, "Cross-Origin-Resource-Policy: cross-origin 없음(미리보기 서버가 보내야 함)"
    return True, "iframe 으로 열림"


LABEL_RE = re.compile(r"^[a-z0-9-]+$")


def embed_allowed(url, tailnet=None, ports=None):
    """(부를 수 있나, 까닭). 이 기계 tailnet 접미사 아래 이름(포트 무관), 또는 PREVIEW_PORTS 의 localhost · 127.0.0.1 만.
    사용자 정보 · 이상한 이름은 거절한다(보드가 아무 곳이나 대신 부르는 창구가 되지 않게)."""
    tailnet = TAILNET if tailnet is None else tailnet
    ports = PREVIEW_PORTS if ports is None else ports
    try:
        u = urllib.parse.urlparse(url or "")
        port = u.port or (443 if u.scheme == "https" else 80)
    except ValueError:
        return False, "주소를 못 읽음"
    host = (u.hostname or "").lower()
    if u.scheme not in ("http", "https") or "@" in u.netloc:
        return False, "http(s) 주소만, 사용자 정보 없이"
    if host in ("localhost", "127.0.0.1"):
        return (True, "") if port in ports else (False, "이 맥은 미리보기 포트(BOARD_PREVIEW_PORTS)만 미리 확인함")
    if tailnet and host.endswith("." + tailnet) and all(LABEL_RE.match(p) for p in host[:-len(tailnet) - 1].split(".")):
        return True, ""
    return False, "이 기계 tailnet(BOARD_TAILNET) 이름 · 미리보기 포트의 이 맥만 미리 확인함"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """리다이렉트는 따라가지 않는다: 허용한 이름이 바깥 · 내부 주소로 넘기는 길(SSRF)을 막는다."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


EMBED_OPENER = urllib.request.build_opener(NoRedirect)


def embed_check(url):
    """보드가 대신 HEAD 를 쳐 본다. 허용 목록(embed_allowed)만, 리다이렉트는 안 따라간다."""
    ok, why = embed_allowed(url)
    if not ok:
        return {"ok": False, "reason": why}
    try:
        with EMBED_OPENER.open(urllib.request.Request(url, method="HEAD"), timeout=5) as r:
            ok, why = embed_verdict(dict(r.headers))
    except urllib.error.HTTPError as e:
        ok, why = False, ("리다이렉트(HTTP %d)는 따라가지 않음" % e.code) if 300 <= e.code < 400 else "HTTP %d" % e.code
    except (urllib.error.URLError, OSError) as e:
        ok, why = False, "미리보기 서버에 닿지 않음(%s)" % getattr(e, "reason", e)
    return {"ok": ok, "reason": why}


def is_owner_note(j):
    """보드가 남긴 소유자 노트: 제목 머리 [fdboard] 이고 세션이 없다(보드는 세션 없이 쓴다). 세션이 [fdboard] 제목을
    흉내 내 쓴 노트는 소유자 노트가 아니다(수신함이 받는 세션의 믿음의 기준이라서)."""
    return (j.get("Title") or "").startswith(NOTE_MARK) and not j.get("SessionID")


# 이 보드를 부르는 이름만(DNS 리바인딩 막기). X-Forwarded-Host 는 믿지 않는다. 이름은 판정의 근거가 아니다
# (위조할 수 있다): 신원은 어느 포트로 왔는지(PORT · LOCAL_PORT)와 access_denied 가 가른다.
# 루프백 밖의 이름(tailnet 이름:포트 등)은 BOARD_HOSTS 로 준다(install.sh 가 tailscale 에서 읽어 적는다).
BOARD_HOSTS = {"127.0.0.1:%d" % PORT, "localhost:%d" % PORT} | {
    h.strip().lower() for h in env("BOARD_HOSTS").split(",") if h.strip()}
LOCAL_HOSTS = {"127.0.0.1:%d" % LOCAL_PORT, "localhost:%d" % LOCAL_PORT}
SECRET_HEADER = "X-Board-Secret"  # 로컬 포트의 쓰기가 지녀야 하는 설치 비밀


def host_ok(headers, hosts=None):
    return (headers.get("Host") or "").strip().lower() in (BOARD_HOSTS if hosts is None else hosts)


def same_origin(headers, hosts=None):
    """쓰기는 이 보드 자신의 페이지에서 온 것만. (통과, 까닭)."""
    sfs = (headers.get("Sec-Fetch-Site") or "").strip()
    origin = (headers.get("Origin") or "").strip()
    if sfs and sfs != "same-origin":
        return False, "Sec-Fetch-Site=%s" % sfs
    if origin:
        hosts = BOARD_HOSTS if hosts is None else hosts
        host = urllib.parse.urlparse(origin).netloc.lower()
        if host not in hosts:
            return False, "Origin %s 이 이 보드(%s)가 아님" % (host, ", ".join(sorted(hosts)))
    if not sfs and not origin:
        return False, "Origin 도 Sec-Fetch-Site 도 없음"
    return True, "같은 출처"


def access_denied(headers, path, method="GET", local=False, auth=None):
    """'' 이면 통과, 아니면 거절 까닭. host_ok 를 지난 요청에만 부른다. Host 이름은 보지 않는다(위조할 수 있다).

    - 바깥 포트(PORT, tailscale serve 가 가리킴): 모든 요청이 serve 가 붙이는 Tailscale-User-Login 을 지니고 그것이
      소유자 로그인 목록에 있어야 한다. 헤더 없음(태그 기기 · Host 위조 포함)은 거절. / 밖(/api/* · /file)은 / 가
      신원 검사 뒤에만 내려 주는 쿠키(설치 때 만든 비밀)를 더 요구한다(다른 사이트가 소유자 브라우저로 부르는 길).
    - 로컬 포트(LOCAL_PORT, 127.0.0.1 고정, serve 가 안 가리킴): 신원 헤더 없이 읽기를 받고, 쓰기는 설치 비밀
      (X-Board-Secret)을 요구한다.
    바깥에서 이 헤더를 위조할 수 없는 것은 두 포트가 루프백에만 묶였을 때다(check_bind).
    같은 사용자의 로컬 프로세스는 비밀 파일을 읽고 헤더를 흉내 낼 수 있다(README 「접근」)."""
    auth = AUTH if auth is None else auth
    if not auth:
        return "접근 설정(%s)이 없음: board/install.sh 를 다시 돌린다" % AUTH_FILE
    if local:
        if method in ("GET", "HEAD"):
            return ""
        given = (headers.get(SECRET_HEADER) or "").strip()
        return "" if given and hmac.compare_digest(given.encode(), auth["secret"].encode()) else \
            "로컬 포트의 쓰기는 %s(설치 비밀)가 있어야 함" % SECRET_HEADER
    login = (headers.get("Tailscale-User-Login") or "").strip().lower()
    if not login:
        return "tailnet 신원(Tailscale-User-Login)이 없음: tailscale serve 로만 부른다(로컬 확인은 %d 포트)" % LOCAL_PORT
    if login not in auth["owner_logins"]:
        return "소유자 로그인이 아님"
    if path in ("/", "/index.html", "/healthz"):
        return ""
    return "" if cookie_ok(headers.get("Cookie"), auth["secret"]) else "보드 쿠키가 없음: / 를 먼저 연다"


def cookie_ok(header, secret):
    for part in (header or "").split(";"):
        name, _, val = part.strip().partition("=")
        if name == COOKIE and hmac.compare_digest(val.encode(), secret.encode()):
            return True
    return False


def check_bind(addr):
    """보드는 루프백에만 묶는다. 바깥 주소에 열면 Tailscale-User-Login 을 누구나 위조할 수 있다."""
    try:
        if ipaddress.ip_address(addr).is_loopback:
            return
    except ValueError:
        if addr == "localhost":
            return
    raise SystemExit("BOARD_BIND_REFUSED: BOARD_ADDR=%s 는 루프백이 아님. 보드는 127.0.0.1 에만 열고 "
                     "바깥에는 tailscale serve 로만 낸다" % addr)


SECRET_WORDS = ("key", "secret", "token", "credential", "password", "passwd", "private", "auth")  # 조각 안 어디든(serviceAccountKey 도), 오탐은 감수
DOT_ALLOWED = {".claude", ".playwright-mcp"}  # 작업자 워크트리 · 스크린숏이 사는 곳


def file_denied(path):
    """허용 목록: 아는 글 · 그림 · 영상 확장자만, 숨김 조각(.git · .certs · .env · .ssh 등) 금지, 비밀 꼴 이름 금지."""
    ext = os.path.splitext(path)[1].lower()
    if ext not in TEXT_TYPES and ext not in MEDIA_TYPES:
        return "보여 줄 수 있는 확장자가 아님: %s" % (ext or "(없음)")
    for root in ROOT_PREFIXES:  # 뿌리 자체(/private/tmp/… 의 private 등)는 우리가 정한 값이라 그 아래 조각만 본다
        if path.startswith(root + os.sep):
            path = path[len(root):]
            break
    for part in path.split(os.sep):
        p = part.lower()
        if not p:
            continue
        if p.startswith(".") and p not in DOT_ALLOWED:
            return "숨김 경로 조각: %s" % part
        if any(w in p for w in SECRET_WORDS) or p.endswith((".pem", ".p12", ".jks", ".keystore")):
            return "비밀 꼴 이름: %s" % part
    return ""


def file_policy(raw):
    """(실제 경로, '') 또는 (None, 까닭). 허용 뿌리 판정은 realpath 로(기호 링크 탈출 금지)."""
    if not raw:
        return None, "path 가 없음"
    p = os.path.expanduser(raw)
    if not os.path.isabs(p):
        return None, "절대 경로만"
    norm = os.path.normpath(p)
    real = os.path.realpath(norm)
    for candidate in (norm, real):
        why = file_denied(candidate)
        if why:
            return None, why
    if not any(real == r or real.startswith(r + os.sep) for r in FILE_ROOTS):
        return None, "허용 뿌리(%s) 밖" % ", ".join(FILE_ROOTS)
    if not os.path.isfile(real):
        return None, "파일이 없음"
    return real, ""


TEXT_TYPES = {".md": "text/markdown", ".markdown": "text/markdown", ".log": "text/plain", ".txt": "text/plain",
              ".json": "application/json", ".jsonl": "application/x-ndjson", ".csv": "text/csv",
              ".gd": "text/plain", ".py": "text/plain", ".sh": "text/plain", ".toml": "text/plain",
              ".yaml": "text/plain", ".yml": "text/plain", ".ts": "text/plain", ".tscn": "text/plain"}
MEDIA_TYPES = {".mp4": "video/mp4", ".m4v": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime",
               ".webp": "image/webp", ".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
               ".gif": "image/gif", ".ogg": "audio/ogg", ".wav": "audio/wav", ".mp3": "audio/mpeg"}


def content_type_of(path):
    ext = os.path.splitext(path)[1].lower()
    if ext in TEXT_TYPES:
        return TEXT_TYPES[ext] + "; charset=utf-8"
    if ext in MEDIA_TYPES:
        return MEDIA_TYPES[ext]
    t = mimetypes.guess_type(path)[0] or "application/octet-stream"
    return t + "; charset=utf-8" if t.startswith("text/") else t


def parse_range(header, size):
    """'bytes=a-b' 하나 -> (start, end) 포함 구간, 없으면 None, 못 맞추면 'bad'."""
    m = re.match(r"^\s*bytes=(\d*)-(\d*)\s*$", header or "")
    if not m or (not m.group(1) and not m.group(2)):
        return None if not header else "bad"
    if m.group(1):
        start = int(m.group(1))
        end = int(m.group(2)) if m.group(2) else size - 1
    else:
        n = int(m.group(2))
        start, end = max(0, size - n), size - 1
    end = min(end, size - 1)
    if start > end or start >= size:
        return "bad"
    return start, end


def tail_bytes(path, lines):
    """파일 끝 lines 줄(바이트). 큰 로그도 끝에서부터만 읽는다."""
    with open(path, "rb") as f:
        f.seek(0, os.SEEK_END)
        pos, buf = f.tell(), b""
        while pos > 0 and buf.count(b"\n") <= lines:
            step = min(CHUNK, pos)
            pos -= step
            f.seek(pos)
            buf = f.read(step) + buf
    return b"\n".join(buf.split(b"\n")[-(lines + 1):])


# ---- fd ------------------------------------------------------------------

def fd_config():
    """(fd 주소, 토큰). 환경 FD_URL · FD_TOKEN 이 있으면 그것, 없으면 fd 클라이언트 설정 파일(요청마다 다시 읽음)."""
    cfg = {}
    try:
        with open(CONFIG) as f:
            cfg = json.load(f)
    except (OSError, ValueError):
        if not (env("FD_URL") and env("FD_TOKEN")):
            raise
    url = env("FD_URL", cfg.get("url") or "http://127.0.0.1:7420")
    return url.rstrip("/"), env("FD_TOKEN", cfg.get("token") or "")


def fd_call(method, path, body=None, **query):
    try:
        base, token = fd_config()
    except (OSError, ValueError) as e:
        return 502, {"error": {"code": "fd_unconfigured",
                               "message": "fd 주소 · 토큰을 못 읽음(%s, 또는 환경 FD_URL · FD_TOKEN): %s" % (CONFIG, e)}}
    url = base + path + ("?" + urllib.parse.urlencode(query, doseq=True) if query else "")  # 목록 값은 되풀이 인자
    headers = {"Authorization": "Bearer " + token} if token else {}
    data = None
    if body is not None:
        data = json.dumps(body, ensure_ascii=False).encode()
        headers["Content-Type"] = "application/json"
        headers["Idempotency-Key"] = "fdboard:" + uuid.uuid4().hex
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=TIMEOUT) as r:
            return r.status, json.load(r)
    except urllib.error.HTTPError as e:
        try:
            out = json.load(e)
        except Exception:
            out = {"error": {"code": "http_%d" % e.code, "message": e.reason}}
        return e.code, out
    except (urllib.error.URLError, OSError, ValueError) as e:
        return 502, {"error": {"code": "fd_unreachable", "message": "flightdeck 서버에 닿지 않음: %s" % e}}


def fd_get(path, **query):
    return fd_call("GET", path, **query)


def fd_post(path, body):
    return fd_call("POST", path, body)


def drop_inbox(session_id, sock):
    """연결이 안 되는 소켓의 받는 곳을 fd 원장에서 지운다(소켓이 같을 때만, README 「세션에 알림」)."""
    return fd_call("DELETE", "/api/v1/sessions/" + urllib.parse.quote(session_id, safe="") + "/inbox", {"socket": sock})


def item_path(iid):
    return "/api/v1/items/" + urllib.parse.quote(iid, safe="")


def owner_note(project, iid, title, body):
    return fd_post("/api/v1/judgments", {"project": project, "kind": NOTE_KIND, "item_id": iid,
                                         "title": "%s %s" % (NOTE_MARK, title), "body": body})


def set_labels(project, iid, add, rm):
    return fd_post(item_path(iid) + "/label", {"project": project, "add": add, "rm": rm})


_project_cache = {}  # project -> (읽은 때, {"path", "repo"})
_projects_list = [0, []]  # [읽은 때, fd 의 프로젝트 목록]


def projects_list():
    """fd /api/v1/projects 의 목록(10분 기억, 200 일 때만 기억해 fd 가 잠깐 안 닿아도 10분씩 비지 않게)."""
    if _projects_list[1] and time.time() - _projects_list[0] < 600:
        return _projects_list[1]
    code, body = fd_get("/api/v1/projects")
    if code != 200:
        return _projects_list[1]
    _projects_list[:] = [time.time(), [p for p in body.get("projects") or [] if isinstance(p, dict)]]
    return _projects_list[1]


def project_of(project):
    """fd 프로젝트의 저장소 폴더와 GitHub 주소(10분 기억). 커밋 링크가 읽는다."""
    hit = _project_cache.get(project)
    if hit and time.time() - hit[0] < 600:
        return hit[1]
    path, url = "", ""
    found_list = projects_list()
    if found_list:
        path = next((p.get("path") for p in found_list if p.get("id") == project), "") or ""
        if path and os.path.isdir(path):
            try:
                out = subprocess.run(["git", "-C", path, "remote", "get-url", "origin"],
                                     capture_output=True, text=True, timeout=5)
                url = github_url(out.stdout)
            except (OSError, subprocess.SubprocessError):
                url = ""
    found = {"path": path if path and os.path.isdir(path) else "", "repo": url}
    if found_list:  # fd 가 안 닿았던 빈 답은 기억하지 않는다
        _project_cache[project] = (time.time(), found)
    return found


def repo_url_of(project):
    return project_of(project)["repo"]


def links_of(raw, project):
    """links 파일 -> 그 프로젝트의 위 링크 줄. 항목에 "project" 가 없으면 모든 프로젝트에, 있으면 그 프로젝트에만."""
    if not isinstance(raw, list):
        raise ValueError("links 파일은 [{\"name\", \"url\", \"project\"?}] 목록이어야 함")
    return [{"name": l.get("name") or l["url"], "url": l["url"]} for l in raw
            if isinstance(l, dict) and l.get("url") and l.get("project") in (None, "", project)]


def read_links(project):
    """(코드, 응답). 파일이 없으면 빈 줄(기계별 설정이라 없어도 된다), 깨졌으면 500 으로 까닭을 낸다."""
    try:
        with open(LINKS) as f:
            return 200, {"links": links_of(json.load(f), project), "file": LINKS}
    except FileNotFoundError:
        return 200, {"links": [], "file": LINKS, "missing": True}
    except (OSError, ValueError, KeyError) as e:
        return 500, err("links_unreadable", "%s 을 못 읽음: %s" % (LINKS, e))


def err(code, message, **extra):
    return {"error": {"code": code, "message": message, **extra}}


# ---- 소유자 동작 (POST) ---------------------------------------------------

def act_note(project, req):
    text = (req.get("text") or "").strip()
    if not text:
        return 400, err("empty", "댓글이 비었음")
    return owner_note(project, req["id"], "소유자 댓글", text)


def act_done(project, req, item):
    if OWNER_TODO not in (item.get("Labels") or []):
        return 409, err("not_owner_turn", "소유자 차례(status:owner) 카드가 아님")
    code, body = set_labels(project, req["id"], [OWNER_DONE], [OWNER_TODO])
    if code != 200:
        return code, body
    return owner_note(project, req["id"], "소유자: 했어", "소유자: 했어")


def act_choose(project, req, item):
    choice = (req.get("choice") or "").strip()
    choices = directives_of(item.get("Body") or "")["choices"]
    if choice not in choices:
        return 400, err("bad_choice", "본문의 선택 보기에 없는 값", choices=choices)
    code, body = owner_note(project, req["id"], CHOICE_PREFIX + choice[:80], CHOICE_PREFIX + choice)
    if code not in (200, 201):
        return code, body
    labels = item.get("Labels") or []
    rm = [l for l in (OWNER_TODO,) if l in labels]
    return set_labels(project, req["id"], [OWNER_DONE], rm)


def act_move(project, req, item):
    to = req.get("to")
    if to == "done":
        return 409, err("done_by_coordinator", "끝남으로는 옮기지 않음: 닫기는 조정자가 커밋과 함께 한다. "
                                               "끝났다고 알리려면 「했어」나 댓글을 남겨 줘.")
    plan = move_labels(to)
    if plan is None:
        return 400, err("bad_column", "모르는 열: %s" % to)
    add, rm = plan
    labels = item.get("Labels") or []
    rm = [l for l in rm if l in labels]
    add = [l for l in add if l not in labels]
    if not add and not rm:
        return 200, {"item": item, "unchanged": True}
    code, body = set_labels(project, req["id"], add, rm)
    if code != 200:
        return code, body
    # 수신함(/api/inbox)에 흔적이 남게: 초인종은 동작 이름만 싣고, 받는 세션은 수신함만 믿는다.
    return owner_note(project, req["id"], MOVE_PREFIX + COLUMN_NAMES[to], MOVE_PREFIX + COLUMN_NAMES[to])


def act_add(project, req):
    title = (req.get("title") or "").strip()
    if not title:
        return 400, err("empty", "제목이 비었음")
    body = (req.get("body") or "").strip() or title
    iid = "owner-" + time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:4]
    return fd_post("/api/v1/items", {"project": project, "id": iid, "title": title,
                                     "body": body + "\n\n(소유자가 보드에서 맡긴 일)", "labels": [OWNER_ASK]})


ACTIONS = {"/api/note": act_note, "/api/done": act_done, "/api/choose": act_choose, "/api/move": act_move}

# 세션 알림(README 「세션에 알림」): 동작마다 초인종에 실을 동작 이름의 열쇠(session_inbox.ACTION_NAMES).
# 소유자 글 · 카드 제목 · 요약은 싣지 않는다. 받는 세션이 /api/inbox 와 fd 에서 읽는다.
TOLD = {"/api/note": "note", "/api/done": "done", "/api/choose": "choose", "/api/move": "move", "/api/add": "add"}


def tell_sessions(project, path, req, body):
    """소유자 동작이 저장된 뒤: 수신함 감시에 적고(inbox_watch), 그 프로젝트의 세션에 초인종 한 줄. 결과는 응답의
    notify 칸에, 실패는 경고와 로그로. 둘은 따로 지킨다(감시가 터져도 초인종은 나간다). 여기서 무엇이 터져도 이미
    저장된 동작의 응답은 그대로 나간다(다시 누르면 노트가 둘이 된다)."""
    if not isinstance(body, dict) or body.get("unchanged"):
        return body
    # 새 카드의 id 는 보드가 지었다: fd 응답의 것만 믿는다(요청 값은 싣지 않는다).
    iid = ((body.get("item") or {}).get("ID") or "") if path == "/api/add" else (req.get("id") or "")
    iid = iid if isinstance(iid, str) else ""
    try:
        WATCH.acted(project, iid, TOLD[path])
    except Exception as e:  # noqa: BLE001
        sys.stderr.write("BOARD_INBOX_WATCH_FAILED %s\n" % e.__class__.__name__)
    try:
        body["notify"] = session_inbox.notify(project, iid, TOLD[path], fd_get, drop_inbox)
    except Exception as e:  # noqa: BLE001  알림은 덤이다: 까닭을 남기고 동작 응답을 지킨다
        sys.stderr.write("BOARD_NOTIFY_FAILED %s\n" % e.__class__.__name__)
        body["notify"] = {"ok": False, "sent": 0, "failed": [], "warning": "알림 못 보냄: 보드 오류 %s" % e.__class__.__name__}
    return body


def unread_or_empty(project):
    """화면의 안 읽힘 경고. 덤이라 터지면 빈 값(보드 화면을 막지 않는다)."""
    try:
        return WATCH.unread(project)
    except Exception as e:  # noqa: BLE001
        sys.stderr.write("BOARD_INBOX_WATCH_FAILED %s\n" % e.__class__.__name__)
        return None


def perform(path, project, req, tell):
    """POST 동작 하나 -> (코드, 응답). tell 이면(소유자 포트) 저장에 성공한 동작을 세션에 알린다(tell_sessions).
    로컬 포트의 쓰기는 에이전트 · 이 맥의 확인이라 「소유자가 했다」고 알리지 않는다."""
    item = None
    if path == "/api/add":
        code, body = act_add(project, req)
    else:
        act = ACTIONS.get(path)
        if not act:
            return 404, err("not_found", path)
        if not req.get("id"):
            return 400, err("missing_id", "id 가 없음")
        if act is act_note:
            code, body = act(project, req)
        else:
            code, detail = fd_get(item_path(req["id"]), project=project)
            if code != 200:
                return code, detail
            item = detail.get("item") or {}
            if item.get("State") in ("done", "dropped"):
                return 409, err("closed", "닫힌 항목은 꼬리표를 바꾸지 않음(댓글은 됨)")
            code, body = act(project, req, item)
    if tell and code in (200, 201):
        body = tell_sessions(project, path, req, body)
    return code, body


def inbox_of(project):
    """조정자가 한 번에 읽는 것: 돌아온 카드(owner-done · owner-ask)와 그 소유자 노트, 최근 소유자 노트."""
    code, dash = fd_get("/api/v1/dashboard.json", project=project)
    if code != 200:
        return code, dash
    items = []
    for it in dash.get("open_items") or []:
        labels = it.get("Labels") or []
        if OWNER_DONE not in labels and OWNER_ASK not in labels:
            continue
        c2, detail = fd_get(item_path(it["ID"]), project=project)
        notes = [j for j in (detail.get("judgments") or []) if is_owner_note(j)] if c2 == 200 else []
        notes.sort(key=lambda j: j.get("At") or "")
        items.append({"id": it["ID"], "title": it.get("Title"), "labels": labels,
                      "kind": "owner-ask" if OWNER_ASK in labels else "owner-done",
                      "body": it.get("Body") or "",
                      "owner_notes": [{"at": j.get("At"), "title": j.get("Title"), "body": j.get("Body")}
                                      for j in notes]})
    code, found = fd_get("/api/v1/judgments", q=NOTE_MARK.strip("[]"), project=project, limit=200)
    recent = []
    if code == 200:
        js = sorted((j for j in found.get("judgments") or [] if is_owner_note(j)),
                    key=lambda j: j.get("At") or "", reverse=True)[:30]
        for j in js:
            target = next((l.get("TargetID") for l in j.get("Links") or [] if l.get("TargetKind") == "item"), "")
            recent.append({"at": j.get("At"), "item": target, "title": j.get("Title"), "body": j.get("Body")})
    return 200, {"project": project, "at": dash.get("at"), "items": items, "recent_owner_notes": recent}


# ---- HTTP ----------------------------------------------------------------

class Handler(BaseHTTPRequestHandler):
    """바깥 포트(PORT). 로컬 포트는 LocalHandler."""
    server_version = "fd-board/3"
    local = False
    hosts = BOARD_HOSTS

    def log_message(self, fmt, *args):  # 경로만, 머리(토큰 없음)는 안 적는다
        sys.stderr.write("%s %s\n" % (self.log_date_time_string(), fmt % args))

    def common_headers(self):
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        # 미리보기(스레드 Godot)를 iframe 안에서 돌리려면 보드도 교차 출처 격리여야 한다.
        self.send_header("Cross-Origin-Opener-Policy", "same-origin")
        self.send_header("Cross-Origin-Embedder-Policy", "credentialless")

    def send(self, code, body, ctype="application/json; charset=utf-8", extra=None):
        data = body if isinstance(body, bytes) else json.dumps(body, ensure_ascii=False).encode()
        self.send_response(code)
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.common_headers()
        self.end_headers()
        self.wfile.write(data)

    def query(self):
        u = urllib.parse.urlparse(self.path)
        return u.path, {k: v[0] for k, v in urllib.parse.parse_qs(u.query).items()}

    def gate(self, path):
        """Host · 신원 · 쿠키 · 비밀. 통과면 False, 거절 응답을 보냈으면 True."""
        if not host_ok(self.headers, self.hosts):
            self.send(421, err("host", "이 보드의 이름으로만 부름"))
            return True
        why = access_denied(self.headers, path, self.command, self.local)
        if why:
            self.send(403, err("access", why))
            return True
        return False

    def do_HEAD(self):
        path, q = self.query()
        if self.gate(path):
            return
        if path == "/file":
            return self.serve_file(q, head=True)
        return self.send(405, err("method", "HEAD 는 /file 만"))

    def do_GET(self):
        path, q = self.query()
        if self.gate(path):
            return
        if path in ("/", "/index.html"):
            with open(os.path.join(HERE, "index.html"), "rb") as f:
                page = f.read()
            if self.local:  # 로컬 포트는 비밀을 내주지 않는다(쓰기는 X-Board-Secret 으로)
                return self.send(200, page, "text/html; charset=utf-8")
            cookie = "%s=%s; HttpOnly; SameSite=Strict; Path=/" % (COOKIE, AUTH["secret"])  # 신원 검사(gate) 뒤에만
            return self.send(200, page, "text/html; charset=utf-8", extra={"Set-Cookie": cookie})
        if path == "/healthz":
            code, body = fd_get("/healthz")
            return self.send(200 if code == 200 else 502, {"ok": code == 200, "fd": body.get("ok", False)})
        if path == "/api/config":
            return self.send(200, {"default_project": DEFAULT_PROJECT})
        if path == "/api/links":
            return self.send(*read_links(q.get("project") or DEFAULT_PROJECT))
        if path == "/api/projects":
            code, body = fd_get("/api/v1/projects")
            if code != 200:
                return self.send(code, body)
            ps = [p["id"] for p in body.get("projects", []) if not p.get("archived") and p.get("items", 0) > 0]
            return self.send(200, {"projects": sorted(ps)})
        project = q.get("project") or DEFAULT_PROJECT
        if path in ("/api/board", "/api/item", "/api/inbox") and not project:
            return self.send(400, err("missing_project", "project 가 없음(?project=<id>, 또는 환경 BOARD_PROJECT)"))
        if path == "/api/board":
            done = max(1, min(int(q.get("done", "30") or 30), 500))
            code, dash = fd_get("/api/v1/dashboard.json", project=project)
            if code != 200:
                return self.send(code, dash)
            add_claimed_items(dash, project)
            code, closed = fd_get("/api/v1/items", project=project, state="closed", limit=done)
            if code != 200:
                return self.send(502, err("closed_list_failed",
                                          "닫힌 항목 목록(GET /api/v1/items)을 못 읽음: %s" % code, fd=closed))
            out = board_of(project, dash, closed, repo_url_of(project))
            out["unread"] = unread_or_empty(project)
            return self.send(200, out)
        if path == "/api/item":
            iid = q.get("id", "")
            if not iid:
                return self.send(400, err("missing_id", "id 가 없음"))
            code, body = fd_get(item_path(iid), project=project)
            if code == 200 and body.get("item"):
                card = card_of(body["item"], set(), repo_url_of(project))
                if card["decision"]:
                    card["decision"]["chosen"] = chosen_of(body.get("judgments"), body["item"].get("Labels"))
                body["card"] = card
            return self.send(code, body)
        if path == "/api/embed":
            ok, why = same_origin(self.headers, self.hosts)
            if not ok:
                return self.send(403, err("cross_origin", "이 보드의 페이지에서 온 요청만: " + why))
            return self.send(200, embed_check(q.get("url", "")))
        if path == "/api/inbox":
            asked = WATCH.clock()  # 읽기 전에 잰다: 읽는 사이 저장된 동작은 이 응답에 없으니 읽힘이 아니다
            code, body = inbox_of(project)
            if code == 200 and self.local:  # 조정 세션은 로컬 포트로 읽는다: 그때까지의 소유자 동작은 읽힘
                WATCH.read(project, at=asked)
            return self.send(code, body)
        if path == "/file":
            return self.serve_file(q)
        return self.send(404, err("not_found", path))

    def serve_file(self, q, head=False):
        real, why = file_policy(q.get("path", ""))
        if not real:
            code = 404 if why == "파일이 없음" else (400 if why in ("path 가 없음", "절대 경로만") else 403)
            return self.send(code, err("file_refused", why))
        ctype = content_type_of(real)
        if q.get("tail"):
            data = tail_bytes(real, max(1, min(int(q["tail"]), 5000)))
            return self.send(200, b"" if head else data, ctype)
        size = os.path.getsize(real)
        rng = parse_range(self.headers.get("Range"), size)
        if rng == "bad":
            self.send_response(416)
            self.send_header("Content-Range", "bytes */%d" % size)
            self.send_header("Content-Length", "0")
            self.common_headers()
            self.end_headers()
            return
        start, end = rng if rng else (0, size - 1)
        length = max(0, end - start + 1)
        self.send_response(206 if rng else 200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(length))
        self.send_header("Accept-Ranges", "bytes")
        if rng:
            self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, size))
        # 파일이 문서로 열려도 이 보드의 출처로 스크립트를 돌리지 못하게.
        self.send_header("Content-Security-Policy", "sandbox")
        self.send_header("Cross-Origin-Resource-Policy", "same-origin")
        self.common_headers()
        self.end_headers()
        if head:
            return
        with open(real, "rb") as f:
            f.seek(start)
            left = length
            try:
                while left > 0:
                    chunk = f.read(min(CHUNK, left))
                    if not chunk:
                        break
                    self.wfile.write(chunk)
                    left -= len(chunk)
            except (BrokenPipeError, ConnectionResetError):
                pass  # 영상 탐색 중 브라우저가 끊는 것은 정상

    def do_POST(self):
        path, q = self.query()
        if self.gate(path):
            return
        ok, why = (True, "") if self.local else same_origin(self.headers, self.hosts)  # 로컬은 gate 가 비밀을 봤다
        if not ok:
            return self.send(403, err("cross_origin", "이 보드의 페이지에서 온 쓰기만 받음: " + why))
        if not (self.headers.get("Content-Type") or "").startswith("application/json"):
            return self.send(415, err("content_type", "application/json 만"))
        n = int(self.headers.get("Content-Length") or 0)
        if n <= 0 or n > MAX_POST:
            return self.send(413, err("size", "본문 크기 %d (1..%d)" % (n, MAX_POST)))
        try:
            req = json.loads(self.rfile.read(n))
            assert isinstance(req, dict)
        except (ValueError, AssertionError):
            return self.send(400, err("bad_json", "JSON 객체가 아님"))
        project = req.get("project") or DEFAULT_PROJECT
        if not project:
            return self.send(400, err("missing_project", "project 가 없음"))
        return self.send(*perform(path, project, req, tell=not self.local))


class LocalHandler(Handler):
    """로컬 포트(LOCAL_PORT): 에이전트 · 로컬 확인용. 신원 헤더 없이 읽기, 쓰기는 설치 비밀."""
    local = True
    hosts = LOCAL_HOSTS


def main():
    check_bind(ADDR)
    if not AUTH:
        raise SystemExit("BOARD_AUTH_MISSING: %s 이 없거나 깨짐(secret 32자 이상). board/install.sh 가 만든다" % AUTH_FILE)
    if LOCAL_PORT == PORT:
        raise SystemExit("BOARD_PORT_CLASH: BOARD_LOCAL_PORT 와 BOARD_PORT 가 같음(%d)" % PORT)
    srv = ThreadingHTTPServer((ADDR, PORT), Handler)
    local = ThreadingHTTPServer(("127.0.0.1", LOCAL_PORT), LocalHandler)
    threading.Thread(target=local.serve_forever, daemon=True).start()
    # 소유자 로그인 · 비밀은 적지 않는다(수만).
    sys.stderr.write("BOARD_READY http://%s:%d local=http://127.0.0.1:%d project=%s hosts=%s roots=%s links=%s "
                     "tailnet=%s preview_ports=%s owners=%d\n" % (
                         ADDR, PORT, LOCAL_PORT, DEFAULT_PROJECT or "(첫째)", ",".join(sorted(BOARD_HOSTS)),
                         ":".join(FILE_ROOTS), LINKS, TAILNET or "(없음)",
                         ",".join(map(str, sorted(PREVIEW_PORTS))) or "(없음)", len(AUTH["owner_logins"])))
    srv.serve_forever()


if __name__ == "__main__":
    main()
