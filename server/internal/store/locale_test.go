package store

import (
	"strings"
	"testing"
)

// goodLocale 는 compose.test.yaml 의 initdb 인자로 만든 DB 가 내는 사실이다(TestPGCheckLocale… 가 실물로 잰다).
var goodLocale = LocaleFacts{ServerVersion: 170012, Provider: "b", Locale: "C.UTF-8", Collate: "C.UTF-8", CType: "C.UTF-8", TrgmReady: true, Trigrams: 7}

// TestJudgeLocale 은 로케일 판정(순수 함수)을 표로 잠근다. 축마다 하나씩 틀린 사실을 넣고, 그 축의 사유가 나오고
// 다른 축의 사유는 안 나오는지 본다. 여러 축이 함께 틀리면 사유가 전부 나와야 한다(한 번에 다 보여야 DB 를 한 번에
// 다시 만든다).
func TestJudgeLocale(t *testing.T) {
	with := func(f func(*LocaleFacts)) LocaleFacts {
		l := goodLocale
		f(&l)
		return l
	}
	for _, c := range []struct {
		name  string
		facts LocaleFacts
		want  []string // 사유에 든 조각. 비면 통과
	}{
		{"기대한 값", goodLocale, nil},
		{"LC_COLLATE 가 C", with(func(l *LocaleFacts) { l.Collate = "C" }), nil},
		{"LC_COLLATE 가 POSIX", with(func(l *LocaleFacts) { l.Collate = "POSIX" }), nil},
		{"LC_CTYPE 의 utf8 표기", with(func(l *LocaleFacts) { l.CType = "C.utf8" }), nil},
		{"PG 18", with(func(l *LocaleFacts) { l.ServerVersion = 180001 }), nil},
		{"libc 제공자", with(func(l *LocaleFacts) { l.Provider = "c" }), []string{"datlocprovider=c"}},
		{"ICU 제공자", with(func(l *LocaleFacts) { l.Provider = "i" }), []string{"datlocprovider=i"}},
		{"builtin 의 다른 로케일", with(func(l *LocaleFacts) { l.Locale = "C" }), []string{`datlocale="C"`}},
		{"LC_COLLATE 가 언어 정렬", with(func(l *LocaleFacts) { l.Collate = "en_US.UTF-8" }), []string{`datcollate="en_US.UTF-8"`}},
		{"LC_CTYPE 가 C", with(func(l *LocaleFacts) { l.CType = "C" }), []string{`datctype="C"`}},
		{"LC_CTYPE 가 UTF-8 아닌 인코딩", with(func(l *LocaleFacts) { l.CType = "ko_KR.EUC-KR" }), []string{`datctype="ko_KR.EUC-KR"`}},
		// 언어 로케일은 UTF-8 이어도 받지 않는다 - 글자 가르기 · 대소문자 접기가 언어 규칙과 libc 판본을 탄다.
		{"LC_CTYPE 가 en_US.UTF-8", with(func(l *LocaleFacts) { l.CType = "en_US.UTF-8" }), []string{`datctype="en_US.UTF-8"`}},
		{"LC_CTYPE 가 ko_KR.utf8@euro", with(func(l *LocaleFacts) { l.CType = "ko_KR.utf8@euro" }), []string{`datctype="ko_KR.utf8@euro"`}},
		{"LC_CTYPE 가 tr_TR.UTF-8", with(func(l *LocaleFacts) { l.CType = "tr_TR.UTF-8" }), []string{`datctype="tr_TR.UTF-8"`}},
		{"pg_trgm 없음", with(func(l *LocaleFacts) { l.TrgmReady, l.Trigrams = false, 0 }), []string{"pg_trgm 이 없어"}},
		{"한글 trigram 0개", with(func(l *LocaleFacts) { l.Trigrams = 0 }), []string{"show_trgm('판단을') 이 비었다"}},
		{"libc C 로 만든 DB", LocaleFacts{ServerVersion: 170012, Provider: "c", Collate: "C", CType: "C", TrgmReady: true},
			[]string{"datlocprovider=c", `datlocale=""`, `datctype="C"`, "show_trgm('판단을') 이 비었다"}},
		// 판이 낮으면 다른 축은 읽지도 않았다(영값) - 「PG 17 이상 필요」 하나만 낸다.
		{"PG 16", LocaleFacts{ServerVersion: 160004}, []string{"PG 17 이상 필요 - 서버가 16.4 다(server_version_num=160004)"}},
		{"판을 못 읽은 영값", LocaleFacts{}, []string{"PG 17 이상 필요"}},
	} {
		err := JudgeLocale(c.facts)
		if len(c.want) == 0 {
			if err != nil {
				t.Errorf("%s: 통과해야 하는데 거절했다: %v", c.name, err)
			}
			continue
		}
		if !IsLocaleRefusal(err) {
			t.Errorf("%s: 거절(*LocaleError)이 아니다: %v", c.name, err)
			continue
		}
		le := err.(*LocaleError)
		if len(le.Reasons) != len(c.want) {
			t.Errorf("%s: 사유 %d개(기대 %d): %v", c.name, len(le.Reasons), len(c.want), le.Reasons)
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: 사유에 %q 가 없다: %v", c.name, w, err)
			}
		}
		if !strings.Contains(err.Error(), "--locale-provider=builtin") {
			t.Errorf("%s: 고치는 법(initdb 인자)이 없다: %v", c.name, err)
		}
	}
}
