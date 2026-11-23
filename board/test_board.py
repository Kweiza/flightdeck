"""board.py 의 순수 규칙 시험. 표준 라이브러리만: python3 -I -m unittest discover -s board -p 'test_*.py'

Go 시험(server/cmd/fd/board_py_test.go)이 이것을 돌리고 「OK」 줄을 단언한다.
"""
import importlib
import os
import sys
import tempfile
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def load_board(**env):
    """환경을 주고 board 를 새로 읽는다(설정은 읽을 때 정해진다)."""
    keep = {k: os.environ.get(k) for k in env}
    os.environ.update(env)
    try:
        sys.path.insert(0, HERE)
        import board
        return importlib.reload(board)
    finally:
        sys.path.remove(HERE)
        for k, v in keep.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v


B = load_board(BOARD_FILE_ROOTS="/nonexistent-root", BOARD_LINKS="/nonexistent/links.json",
               FD_CONFIG="/nonexistent/config.json", BOARD_PROJECT="", BOARD_HOSTS="board.example.ts.net:37430",
               BOARD_AUTH="/nonexistent/auth.json", BOARD_TAILNET="", BOARD_PREVIEW_PORTS="", BOARD_ADDR="", BOARD_PORT="")

DECISION = """맨 윗줄 요약
### 질문
붉은 테두리를 쓸까?
지금 정해야 카드 그림을 뽑는다.
### 배경
- 지금 테두리는 회색
### 보기: 붉은 테두리 (추천)
눈에 띈다.
그림: /x/red.png
### 보기: 회색 그대로
일이 없다.
### 추천
붉은 테두리가 낫다.
Claude: 붉은 테두리, 구매 버튼이 산다
- Codex: 회색 그대로, 덜 시끄럽다
선택: 붉은 테두리 (추천) | 회색 그대로
"""


class Directives(unittest.TestCase):
    def test_lines(self):
        d = B.directives_of("미리보기: https://p.ts.net:1/\n그림: /a.png\n영상: /b.mp4\n파일: /c.md\n"
                            "링크: 콘솔 | https://play.google.com/\n커밋: abc1234 def5678\n"
                            "소유자: 누를 것 · 탭 · 버튼\n선택: A | B\n미리보기: 상대주소", "https://github.com/o/r")
        self.assertEqual(d["previews"], ["https://p.ts.net:1/"])
        self.assertEqual(d["images"], ["/a.png"])
        self.assertEqual(d["videos"], ["/b.mp4"])
        self.assertEqual(d["files"], ["/c.md"])
        self.assertEqual(d["links"], [{"name": "콘솔", "url": "https://play.google.com/"}])
        self.assertEqual([c["url"] for c in d["commits"]],
                         ["https://github.com/o/r/commit/abc1234", "https://github.com/o/r/commit/def5678"])
        self.assertEqual(d["owner"], ["누를 것 · 탭 · 버튼"])
        self.assertEqual(d["choices"], ["A", "B"])

    def test_github_url(self):
        for remote in ("git@github.com:team/game-x.git", "https://github.com/team/game-x", "ssh://git@github.com/team/game-x.git"):
            self.assertEqual(B.github_url(remote), "https://github.com/team/game-x")
        self.assertEqual(B.github_url("git@gitlab.com:a/b.git"), "")


class Decision(unittest.TestCase):
    def test_four_parts(self):
        d = B.decision_of(DECISION, B.directives_of(DECISION)["choices"])
        self.assertEqual(d["question"], "붉은 테두리를 쓸까?\n지금 정해야 카드 그림을 뽑는다.")
        self.assertEqual(d["background"], "- 지금 테두리는 회색")
        self.assertEqual([o["name"] for o in d["options"]], ["붉은 테두리", "회색 그대로"])
        self.assertEqual([o["choice"] for o in d["options"]], ["붉은 테두리 (추천)", "회색 그대로"])
        self.assertEqual(d["options"][0]["body"], "눈에 띈다.")  # 그림: 줄은 절 글에서 빠진다
        self.assertEqual(d["recommended"], "붉은 테두리")
        self.assertEqual([(j["who"], j["pick"]) for j in d["judges"]], [("Claude", "붉은 테두리"), ("Codex", "회색 그대로")])
        self.assertTrue(d["split"])
        self.assertEqual(d["recommendation"], "붉은 테두리가 낫다.")

    def test_card_summary_is_question(self):
        card = B.card_of({"ID": "x", "Body": DECISION, "Labels": ["status:owner", "ui"], "State": "open"}, set())
        self.assertEqual(card["summary"], "붉은 테두리를 쓸까?")
        self.assertEqual(card["column"], "owner")
        self.assertEqual(card["labels"], ["ui"])

    def test_old_form(self):
        body = "고를 것\n선택: A안 | B안"
        self.assertIsNone(B.decision_of(body, B.directives_of(body)["choices"]))
        self.assertEqual(B.card_of({"ID": "y", "Body": body}, set())["choices"], ["A안", "B안"])

    def test_chosen(self):
        js = [{"Title": "[fdboard] 소유자 선택: A", "Body": "소유자 선택: A", "At": "2026-11-23T01:00:00Z"},
              {"Title": "[fdboard] 소유자 선택: B", "Body": "소유자 선택: B", "At": "2026-11-23T02:00:00Z"},
              {"Title": "남의 노트", "Body": "소유자 선택: C", "At": "2026-11-23T03:00:00Z"}]
        self.assertEqual(B.chosen_of(js, ["status:owner-done"]), "B")
        self.assertEqual(B.chosen_of(js, ["status:owner"]), "")  # 조정자가 다시 묻는 중


class Claims(unittest.TestCase):
    def test_str_and_dict(self):
        dash = {"sessions": [
            {"view": {"Claims": ["a", {"ItemID": "b"}, {"ItemID": "c", "ReleasedAt": "2026-11-23T00:00:00Z"}]}},
            {"view": {"Claims": None}}, {}]}
        self.assertEqual(B.claimed_ids(dash), {"a", "b"})

    def test_column(self):
        self.assertEqual(B.column_of({"ID": "a", "State": "open"}, {"a"}), "running")
        self.assertEqual(B.column_of({"ID": "a", "State": "done", "Labels": ["status:running"]}, {"a"}), "done")
        self.assertEqual(B.column_of({"ID": "a", "State": "open", "Labels": ["status:owner-ask"]}, set()), "owner")
        self.assertEqual(B.column_of({"ID": "a", "State": "open"}, set()), "waiting")

    def test_move(self):
        self.assertEqual(B.move_labels("waiting"), ([], list(B.STATUS_LABELS)))
        self.assertIsNone(B.move_labels("done"))


class Files(unittest.TestCase):
    def test_private_tmp_root(self):
        roots, prefixes = B.roots_of("/private/tmp/claude-1000")
        self.assertIn("/tmp/claude-1000", prefixes)  # /tmp 로 와도 뿌리를 뗀다
        keep = B.ROOT_PREFIXES
        B.ROOT_PREFIXES = prefixes
        try:
            self.assertEqual(B.file_denied("/private/tmp/claude-1000/proj/sess/scratchpad/shot.png"), "")
            self.assertEqual(B.file_denied("/tmp/claude-1000/proj/sess/scratchpad/shot.png"), "")
            self.assertIn("비밀", B.file_denied("/private/var/shot.png"))  # 뿌리 밖의 private 는 그대로 막힌다
        finally:
            B.ROOT_PREFIXES = keep

    def test_refusals(self):
        self.assertIn("숨김", B.file_denied("/r/.certs/firebase/app.json"))
        self.assertIn("숨김", B.file_denied("/r/.env.png"))
        self.assertIn("숨김", B.file_denied("/r/repo/.git/HEAD.txt"))
        self.assertEqual(B.file_denied("/r/.claude/worktrees/a/shot.png"), "")
        self.assertIn("비밀", B.file_denied("/r/serviceAccountKey.json"))
        self.assertIn("확장자", B.file_denied("/r/notes.pdf"))

    def test_symlink_escape(self):
        with tempfile.TemporaryDirectory() as root, tempfile.TemporaryDirectory() as outside:
            inside = os.path.join(root, "shot.png")
            open(inside, "wb").close()
            secret = os.path.join(outside, "leak.png")
            open(secret, "wb").close()
            link = os.path.join(root, "link.png")
            os.symlink(secret, link)
            keep = B.FILE_ROOTS, B.ROOT_PREFIXES
            B.FILE_ROOTS, B.ROOT_PREFIXES = B.roots_of(root)
            try:
                self.assertEqual(B.file_policy(inside), (os.path.realpath(inside), ""))
                real, why = B.file_policy(link)
                self.assertIsNone(real)
                self.assertTrue(why)
                self.assertEqual(B.file_policy("shot.png"), (None, "절대 경로만"))
                self.assertEqual(B.file_policy(os.path.join(root, "none.png")), (None, "파일이 없음"))
            finally:
                B.FILE_ROOTS, B.ROOT_PREFIXES = keep


class Config(unittest.TestCase):
    def test_hosts(self):
        self.assertTrue(B.host_ok({"Host": "127.0.0.1:7430"}))
        self.assertTrue(B.host_ok({"Host": "board.example.ts.net:37430"}))
        self.assertFalse(B.host_ok({"Host": "evil.example:7430"}))
        self.assertFalse(B.same_origin({"Origin": "https://evil.example"})[0])
        self.assertTrue(B.same_origin({"Sec-Fetch-Site": "same-origin"})[0])

    def test_no_default_project(self):
        self.assertEqual(B.DEFAULT_PROJECT, "")

    def test_links(self):
        raw = [{"name": "모두", "url": "https://a/"}, {"name": "게임", "url": "https://b/", "project": "game"},
               {"name": "남", "url": "https://c/", "project": "other"}, {"name": "주소 없음"}]
        self.assertEqual([l["name"] for l in B.links_of(raw, "game")], ["모두", "게임"])
        self.assertEqual(B.read_links("game"), (200, {"links": [], "file": "/nonexistent/links.json", "missing": True}))
        with self.assertRaises(ValueError):
            B.links_of({"name": "x"}, "game")


OWNER = "owner@example.com"
SECRET = "s" * 40
AUTH = {"secret": SECRET, "owner_logins": [OWNER]}
TS = "board.example.ts.net:37430"


class Embed(unittest.TestCase):
    def test_allowlist(self):
        ok = lambda u: B.embed_allowed(u, "tail0.ts.net", {8000})[0]
        self.assertTrue(ok("https://mac.tail0.ts.net:30818/"))
        self.assertFalse(ok("https://mac.othertail.ts.net/"))  # 남의 tailnet
        self.assertFalse(ok("https://mac.tail0.ts.net.evil.example/"))
        self.assertFalse(ok("https://user@mac.tail0.ts.net/"))
        self.assertFalse(ok("http://127.0.0.1:7420/"))  # fd 서버 같은 이 맥의 다른 포트
        self.assertTrue(ok("http://localhost:8000/"))
        self.assertFalse(ok("file:///etc/passwd"))
        self.assertFalse(B.embed_allowed("https://mac.tail0.ts.net/", "", set())[0])  # tailnet 미설정이면 안 부름

    def test_redirect_not_followed(self):
        import http.server
        import threading
        hits = []

        class H(http.server.BaseHTTPRequestHandler):
            def do_HEAD(self):
                hits.append(self.path)
                self.send_response(302)
                self.send_header("Location", "http://127.0.0.1:%d/next" % self.server.server_port)
                self.end_headers()

            def log_message(self, *a):
                pass

        srv = http.server.HTTPServer(("127.0.0.1", 0), H)
        t = threading.Thread(target=srv.serve_forever, daemon=True)
        t.start()
        keep = B.PREVIEW_PORTS
        B.PREVIEW_PORTS = {srv.server_port}
        try:
            v = B.embed_check("http://127.0.0.1:%d/start" % srv.server_port)
        finally:
            B.PREVIEW_PORTS = keep
            srv.shutdown()
            srv.server_close()
        self.assertFalse(v["ok"])
        self.assertIn("리다이렉트", v["reason"])
        self.assertEqual(hits, ["/start"])  # /next 로 안 갔다


class Access(unittest.TestCase):
    def deny(self, path, auth=AUTH, **h):
        return B.access_denied({k.replace("_", "-"): v for k, v in h.items()}, path, auth)

    def test_tailnet_needs_owner(self):
        self.assertIn("신원", self.deny("/", Host=TS))
        self.assertIn("소유자", self.deny("/", Host=TS, Tailscale_User_Login="other@example.com"))
        self.assertEqual(self.deny("/", Host=TS, Tailscale_User_Login="Owner@Example.com"), "")
        self.assertIn("쿠키", self.deny("/api/board", Host=TS, Tailscale_User_Login=OWNER))
        self.assertEqual(self.deny("/api/board", Host=TS, Tailscale_User_Login=OWNER, Cookie="a=b; fdboard=" + SECRET), "")

    def test_loopback_needs_cookie(self):
        self.assertEqual(self.deny("/", Host="127.0.0.1:7430"), "")
        self.assertIn("쿠키", self.deny("/file", Host="127.0.0.1:7430"))
        self.assertIn("쿠키", self.deny("/api/inbox", Host="127.0.0.1:7430", Cookie="fdboard=wrong"))
        self.assertEqual(self.deny("/api/inbox", Host="127.0.0.1:7430", Cookie="fdboard=" + SECRET), "")
        self.assertIn("소유자", self.deny("/", Host="127.0.0.1:7430", Tailscale_User_Login="other@example.com"))

    def test_no_auth_file(self):
        self.assertIn("install.sh", B.access_denied({"Host": "127.0.0.1:7430"}, "/", {}))
        self.assertIsNone(B.read_auth("/nonexistent/auth.json"))

    def test_bind_loopback_only(self):
        self.assertEqual(B.ADDR, "127.0.0.1")
        B.check_bind("127.0.0.1")
        B.check_bind("::1")
        for bad in ("0.0.0.0", "100.64.0.1", "::"):
            with self.assertRaises(SystemExit):
                B.check_bind(bad)


class Http(unittest.TestCase):
    """실제 Handler 를 띄워 쿠키 · embed 같은 출처 검사를 본다(fd 는 안 부르는 경로만)."""

    @classmethod
    def setUpClass(cls):
        import threading
        cls.keep = B.AUTH, B.Handler.log_message
        B.AUTH = AUTH
        B.Handler.log_message = lambda *a: None
        cls.srv = B.ThreadingHTTPServer(("127.0.0.1", 0), B.Handler)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()
        B.AUTH, B.Handler.log_message = cls.keep

    def get(self, path, **h):
        import http.client
        c = http.client.HTTPConnection("127.0.0.1", self.srv.server_port, timeout=5)
        c.request("GET", path, headers={"Host": "127.0.0.1:7430", **{k.replace("_", "-"): v for k, v in h.items()}})
        r = c.getresponse()
        r.read()
        c.close()
        return r

    def test_index_sets_strict_cookie(self):
        r = self.get("/")
        self.assertEqual(r.status, 200)
        self.assertEqual(r.getheader("Set-Cookie"), "fdboard=%s; HttpOnly; SameSite=Strict; Path=/" % SECRET)

    def test_file_and_api_need_cookie(self):
        self.assertEqual(self.get("/file?path=/x.png").status, 403)
        self.assertEqual(self.get("/api/config").status, 403)
        self.assertEqual(self.get("/api/config", Cookie="fdboard=" + SECRET).status, 200)
        self.assertEqual(self.get("/", Host="evil.example:7430").status, 421)

    def test_embed_same_origin(self):
        ck = "fdboard=" + SECRET
        self.assertEqual(self.get("/api/embed?url=https://x/", Cookie=ck).status, 403)
        self.assertEqual(self.get("/api/embed?url=https://x/", Cookie=ck, Sec_Fetch_Site="cross-site").status, 403)
        self.assertEqual(self.get("/api/embed?url=https://x/", Cookie=ck, Sec_Fetch_Site="same-origin").status, 200)


if __name__ == "__main__":
    unittest.main()
