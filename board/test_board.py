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
               FD_CONFIG="/nonexistent/config.json", BOARD_PROJECT="", BOARD_HOSTS="board.example.ts.net:37430")

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


if __name__ == "__main__":
    unittest.main()
