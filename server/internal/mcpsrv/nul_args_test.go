package mcpsrv

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// MCP 도구 인자는 REST 로 넘기기 전에 풀린다(decodeArgs). 풀기는 짝 없는 서로게이트 · 깨진 UTF-8 을 U+FFFD 로 바꿔 흔적을 지우므로,
// 그런 인자는 **풀기 전에** 원문으로 거절한다 - 백엔드는 한 번도 안 불리고(쓰기 0), 신호는 남는다(살아 있는 세션의 호출이다).
// 고치기 전에는 그 글이 U+FFFD 가 든 다른 글로 원장에 영구히 남았다.
func TestCallToolRefusesArgsThatDecodingWouldRewrite(t *testing.T) {
	for name, raw := range map[string]string{
		"짝 없는 서로게이트": `{"kind":"ask","body":"본문\ud800끝","item_id":"x"}`,
		"뒤집힌 짝":      `{"kind":"ask","body":"본문\udc00\ud800","item_id":"x"}`,
		"깨진 UTF-8":   `{"kind":"ask","body":"본문` + "\xff" + `","item_id":"x"}`,
		"키의 U+0000":  `{"kind":"ask","body":"본문","\u0000":"x"}`,
	} {
		spy := &noteSpyBackend{}
		res := gatedServer(spy).callTool(context.Background(), "note", json.RawMessage(raw))
		if spy.called {
			t.Errorf("%s: 백엔드 Note 가 불렸다 - U+FFFD 로 바뀐 글이 원장에 남는다", name)
		}
		if !res.IsError || !strings.Contains(resText(res), "다시 불러라") {
			t.Errorf("%s: 거절이 오류로 안 나갔다:\n%s", name, resText(res))
		}
		if spy.beats == 0 {
			t.Errorf("%s: 거절하면서 mcp 신호를 안 남겼다", name)
		}
	}
}

// 반대 갈래: 풀기가 바꾸지 않는 인자(자유 글의 U+0000, 올바른 서로게이트 짝, `\u003c` 같은 다른 이스케이프)는 그대로 흐른다 -
// 자유 글의 NUL 은 REST 가 받아 원장 칸 관문이 `\x00` 표기로 옮긴다.
func TestCallToolPassesArgsThatDecodingKeeps(t *testing.T) {
	for name, raw := range map[string]string{
		"자유 글의 U+0000": `{"kind":"ask","body":"본문\u0000끝","item_id":"x"}`,
		"서로게이트 짝":      `{"kind":"ask","body":"본문\ud83d\ude00","item_id":"x"}`,
		"다른 이스케이프":     `{"kind":"ask","body":"\u003cb\u003e \\ud800 글자","item_id":"x"}`,
	} {
		spy := &noteSpyBackend{}
		res := gatedServer(spy).callTool(context.Background(), "note", json.RawMessage(raw))
		if !spy.called || res.IsError {
			t.Errorf("%s: 막았다:\n%s", name, resText(res))
		}
	}
}
