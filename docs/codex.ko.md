[English](codex.md) | **한국어**

# codex 에서 쓰기

[← README](../README.ko.md)

---

같은 보드에 codex 세션을 올릴 수 있다. 겹침 처방이 두 하네스를 함께 보는 것이 이 기능의 목적이다.

```bash
fd setup --install-codex
```

이것이 깔아 주는 것은 셋이다 — 고정 경로 래퍼 `~/.local/bin/fd-hook`, **`fd` 그 자체
(`~/.local/bin/fd`)**, 그리고 `~/.codex/hooks.json`.
**기존 `hooks.json` 은 절대 안 덮는다.** 이미 있으면 넣을 내용을 화면에 내고 멈추므로 손으로 병합해라.

## ★ 그리고 반드시 TUI 를 한 번 띄워라

```bash
codex        # "Hooks need review" 를 통과시킨다
```

**이것을 안 하면 훅은 한 번도 안 돈다.** codex 는 신뢰받지 않은 훅을 **조용히 건너뛴다** —
`codex exec` 로만 쓰면 승인 화면을 볼 기회가 영영 없고, 보드에 카드가 안 뜨는 이유를
어디에서도 알 수 없다. 로그에 훅 얘기가 **한 줄도 안 나온다.**

신뢰가 붙으면 반대로 로그가 말해 준다 — `hook: SessionStart` … `hook: SessionStart Completed`.
그 줄의 유무가 곧 신뢰 여부다.

## 왜 훅 명령이 그렇게 생겼나

신뢰는 `~/.codex/config.toml` 에 이렇게 박힌다:

```toml
[hooks.state."/Users/…/.codex/hooks.json:session_start:0:0"]
trusted_hash = "sha256:086dc4d6…"
```

**그 해시는 훅 정의(명령 문자열)만 본다.** 스크립트 내용은 안 본다 — 명령을 한 글자 바꾸면
신뢰가 깨지고, 원복하면 내용을 통째로 갈아도 다시 돈다.

그래서 훅 명령이 `~/.local/bin/fd-hook` 이라는 **고정 경로**를 부른다. 여기에
`${CLAUDE_PLUGIN_ROOT}/bin/fd` 처럼 버전이 든 경로를 쓰면 **fd 를 판올림할 때마다 TUI 재승인**이고,
재승인 전까지 훅이 조용히 죽는다. 래퍼가 그 안에서 설치본 중 최신 판을 고르므로
**판올림해도 명령 문자열은 안 바뀐다.**

> 이 래퍼는 **정식 플러그인 설치본만** 고른다. 저장소 체크아웃은 일부러 안 본다 —
> 그러면 낡은 판이 최신인 척 돌고 아무도 모른다.

## 안 되는지 재는 법

```bash
fd doctor        # ■ codex 절
```

네 축을 이름으로 낸다: 훅 파일 · **훅 신뢰** · 훅 명령(고정 경로인가) · 훅 래퍼.
신뢰가 없으면 그 줄이 `✗` 로 뜨고 무엇을 해야 하는지 말한다. **무출력은 통과가 아니다** —
codex 자신의 `codex doctor` 는 훅을 한 마디도 안 재므로(체크 19개 어디에도 없다) 이 화면이
유일한 관측 창구다.

## 샌드박스 네트워크

codex 기본 샌드박스는 네트워크를 끊고, 그 상태의 fd 는 서버에 **통째로 못 붙는다**
(`connect: operation not permitted`).

```bash
codex -c sandbox_workspace_write.network_access=true
```

또는 `~/.codex/config.toml` 에 박아라. 이것은 당신의 샌드박스 정책을 여는 일이다 —
무엇을 왜 여는지 알고 켜라.

## codex 에서 지금 되는 것과 안 되는 것

| codex 에서 | 오늘 |
|---|---|
| 훅 — 세션 카드 · 발자국 · 겹침 처방 · 배너 | ✅ 돈다 |
| 터미널 `fd` | ✅ 깔린다 — **PATH 는 당신이 넣어야 한다**(아래) |
| 응답 꼬리(겹침·미확인) | ✅ 쓰기 명령 전부에 붙는다 |
| `pick --leave` · `finish --followups` · `land --resource` | ✅ 있다 |
| 처방문의 도구 문법 | ✅ codex 카드에는 `fd …` 로 나온다 |
| MCP 도구 10개 | ❌ **안 만든다**(설계 판정) |

### PATH 를 넣어야 한다 — 이것만 사람이 한다

`fd setup --install-codex` 가 `~/.local/bin/fd` 를 깔지만, **그 디렉토리는 이 머신의 깨끗한
로그인 셸 PATH 에 없다**(2026-08-31 실측: 항목 20개 어디에도 없다). 그래서 설치 직후
화면이 이 줄을 낸다:

```bash
export PATH="$HOME/.local/bin:$PATH"   # ~/.zshrc 등에 넣어라
```

넣기 전에도 절대경로로는 바로 된다:

```bash
~/.local/bin/fd board
~/.local/bin/fd note --kind decision --title "…" --body -
```

> ⚠️ `fd` 라는 이름은 `fd-find`(find 대체제)와 겹친다. 그쪽이 PATH 앞에 있으면 그쪽이 돈다 —
> `fd doctor` 의 「codex 창의 fd」 축이 **무엇이 먼저 잡히는지 경로로** 말한다. 별칭을 다른
> 이름으로 잡거나 `~/.local/bin` 을 앞에 둬라.

### 깔린 `fd` 는 판올림을 따라간다

래퍼와 **같은 스크립트**다 — 설치본(`~/.claude/plugins/cache/*/flightdeck/*/bin/fd`) 중
가장 높은 판을 골라 exec 한다. 그래서 플러그인을 판올림해도 이 파일을 다시 깔 필요가 없고,
훅 신뢰도 안 깨진다(명령 문자열이 안 바뀐다).

### MCP 를 안 만드는 이유

codex 는 MCP 자식에게 코어 13개(HOME·PATH·PWD 등)만 주고 **세션 id 를 안 준다.** 그래서 MCP
도구는 자기가 어느 창의 것인지 모르고, 가릴 방법도 없다 — 부모 codex 프로세스의 환경에도
세션 id 가 없고(실측), cwd 는 같은 저장소의 두 창이 똑같다.

**정체 없는 MCP 를 붙이면 창 둘이 카드 한 장을 공유해 원장이 거짓말한다.**

그리고 중요한 것 하나 — **MCP 를 만들어도 위 표의 결손이 안 메워진다.** `fd` 가 안 깔린 것은
설치의 문제이고 꼬리·손잡이는 CLI 표면의 문제다. MCP 는 그 어느 것도 고치지 않고 대신 정체
없는 카드라는 새 거짓을 들여온다. 그래서 안 만든다 — 판정과 **뒤집히는 조건 넷**은 DESIGN
「하네스 축」 절에 있다.
