package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ─────────────────────────────────────────────────────────────────────────────
// DB 로케일 관문 - 판정은 순수 함수(JudgeLocale), 읽기는 readLocale
// ─────────────────────────────────────────────────────────────────────────────
//
// 판단 검색(pg_trgm ILIKE · lower)과 정렬 결과가 DB 의 로케일에 기댄다. 그런데 로케일은 DB 를 만들 때(initdb ·
// CREATE DATABASE) 정해지고 그 뒤로는 못 바꾼다 - 스키마 SQL(pg/bootstrap.sql · 기준선)이 고칠 수 없는 축이다.
// 그래서 서버가 따로 잰다. 재는 자리는 넷이고 판정은 이 파일의 JudgeLocale 하나다.
//   - 기동(OpenWithLogger): 판정이 거절이면 서버가 뜨지 않는다.
//   - 이전 실행기(fd migrate): 빈 DB 면 기준선을 까는 트랜잭션 안에서(pg_trgm 이 그 안에서 생긴다), 아니면 백업 · 증분 전에.
//   - 판 재기(ProbeMigration): 거절로 낸다 - fd selfcheck 가 「이 판으로 다시 떠도 되는가」를 같은 판정으로 답한다.
//   - /healthz 의 db_locale_ok: 지금 붙은 DB 를 다시 잰다(연결 풀이 다시 붙은 서버가 다른 DB 일 수 있다).
//
// SQLite 판은 로케일이 없다(정렬은 BINARY, 검색 낱말 가르기는 FTS5 unicode61 이 엔진 안에 있다) - 늘 통과다.

// trigramProbe 는 한글 trigram 이 나오는지 재는 표본이다. 로케일이 한글을 글자로 안 보면 0개가 된다.
const trigramProbe = "판단을"

// LocaleFacts 는 Postgres 원장 DB 의 로케일 사실이다. readLocale 이 읽고 JudgeLocale 이 판정한다.
type LocaleFacts struct {
	Provider string // pg_database.datlocprovider: b(builtin) · c(libc) · i(icu)
	Locale   string // datlocale - builtin · icu 제공자의 기본 로케일. libc 면 빈 문자열
	Collate  string // datcollate - libc LC_COLLATE
	CType    string // datctype - libc LC_CTYPE. PG17 의 pg_trgm 은 제공자가 아니라 이것으로 글자를 가른다
	// TrgmReady 는 pg_trgm 의 show_trgm 이 이 DB 에 있는가다. 없으면 Trigrams 는 값이 아니다.
	TrgmReady bool
	Trigrams  int // cardinality(show_trgm(trigramProbe))
}

// LocaleError 는 로케일 관문의 거절이다. Reasons 는 걸린 축마다 하나다(하나 이상).
type LocaleError struct{ Reasons []string }

func (e *LocaleError) Error() string {
	return "DB 로케일이 이 서버가 기대는 값이 아니다 - " + strings.Join(e.Reasons, " · ") +
		" (DB 를 initdb --locale-provider=builtin --builtin-locale=C.UTF-8 --lc-collate=C.UTF-8 --lc-ctype=C.UTF-8 -E UTF8 로 " +
		"만든 클러스터에 새로 만들고 옮겨라. 로케일은 만든 뒤에 못 바꾼다)"
}

// cCollations 는 코드포인트(바이트) 순으로 정렬하는 libc LC_COLLATE 다.
var cCollations = map[string]bool{"C": true, "POSIX": true, "C.UTF-8": true, "C.utf8": true}

// JudgeLocale 은 로케일 사실이 이 서버가 기대는 값인지 판정한다. 순수 함수다. nil 이면 통과다.
//
// 기대하는 값(compose.test.yaml 의 initdb 인자가 만드는 DB):
//   - 제공자 builtin(b), 기본 로케일 C.UTF-8. 정렬이 코드포인트 순이라 SQLite 의 BINARY 와 ORDER BY 결과가 같고,
//     lower · ILIKE 의 대소문자 접기가 libc · ICU 판본을 안 탄다.
//   - LC_COLLATE 가 C 계열(코드포인트 순). builtin 이면 기본 정렬은 이것을 안 쓰지만, 정렬 규칙이 libc 로 새는 자리
//     (COLLATE "default" 가 아닌 libc 정렬)가 생겨도 순서가 같게 둔다.
//   - LC_CTYPE 가 UTF-8 libc 로케일(C · POSIX 가 아니다), 그리고 show_trgm(trigramProbe) 가 비지 않는다. pg_trgm 이
//     이것으로 글자를 가르므로 `C` 면 한글 trigram 이 0개, 한글 검색이 조용히 0건이 된다. 이름만으로는 그 로케일이
//     이 기계에서 한글을 글자로 보는지 모르니 실제로 잰다.
//
// ★ 걸린 축을 다 모아 낸다. 처음 하나에서 멈추면 고친 뒤 다음 축에서 또 멈춘다 - 로케일은 DB 를 새로 만들어야
// 고쳐지므로 한 번에 다 보여야 한다.
func JudgeLocale(f LocaleFacts) error {
	var bad []string
	if f.Provider != "b" {
		bad = append(bad, fmt.Sprintf("정렬 제공자가 builtin 이 아니다(datlocprovider=%s) - 정렬과 대소문자 접기가 제공자 판본을 탄다", f.Provider))
	}
	if f.Locale != "C.UTF-8" {
		bad = append(bad, fmt.Sprintf("기본 로케일이 C.UTF-8 이 아니다(datlocale=%q) - 정렬이 코드포인트 순이 아니다", f.Locale))
	}
	if !cCollations[f.Collate] {
		bad = append(bad, fmt.Sprintf("LC_COLLATE 가 C 계열이 아니다(datcollate=%q)", f.Collate))
	}
	if !isUTF8Locale(f.CType) {
		bad = append(bad, fmt.Sprintf("LC_CTYPE 가 UTF-8 로케일이 아니다(datctype=%q) - pg_trgm 이 한글을 글자로 안 본다", f.CType))
	}
	switch {
	case !f.TrgmReady:
		bad = append(bad, "pg_trgm 이 없어 show_trgm 을 못 잰다 - 판단 검색 인덱스가 그것을 쓴다(준비 bootstrap.sql 이 깐다)")
	case f.Trigrams == 0:
		bad = append(bad, fmt.Sprintf("show_trgm('%s') 이 비었다 - 이 DB 에서는 한글 검색이 0건이 된다(datctype=%q)", trigramProbe, f.CType))
	}
	if len(bad) == 0 {
		return nil
	}
	return &LocaleError{Reasons: bad}
}

// isUTF8Locale 은 libc 로케일 이름이 UTF-8 인코딩인가다. 순수 함수다. `언어_지역.인코딩@변형` 꼴에서 인코딩만 본다.
// C · POSIX 처럼 인코딩이 없는 이름은 거짓이다.
func isUTF8Locale(name string) bool {
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return false
	}
	enc := strings.ToLower(name[i+1:])
	return enc == "utf-8" || enc == "utf8"
}

// readLocale 은 지금 붙은 DB 의 로케일 사실을 읽는다. 판정은 안 한다(JudgeLocale).
//
// ★ show_trgm 은 따로 묻는다. 확장이 없으면 그 함수 이름이 든 문장은 해석 단계에서 실패하고(42883), 트랜잭션 안이면
// 그 트랜잭션을 버린다. 있는지 먼저 물어 사실(TrgmReady)로 낸다.
func readLocale(ctx context.Context, q Reader) (LocaleFacts, error) {
	var f LocaleFacts
	if err := q.QueryRowContext(ctx, `SELECT datlocprovider::text, coalesce(datlocale, ''), datcollate, datctype,
			to_regprocedure('show_trgm(text)') IS NOT NULL
		FROM pg_catalog.pg_database WHERE datname = current_database()`).
		Scan(&f.Provider, &f.Locale, &f.Collate, &f.CType, &f.TrgmReady); err != nil {
		return f, fmt.Errorf("DB 로케일을 읽지 못했다: %w", err)
	}
	if !f.TrgmReady {
		return f, nil
	}
	if err := q.QueryRowContext(ctx, `SELECT cardinality(show_trgm(?))`, trigramProbe).Scan(&f.Trigrams); err != nil {
		return f, fmt.Errorf("show_trgm 을 재지 못했다: %w", err)
	}
	return f, nil
}

// localeGate 는 q 가 붙은 DB 의 로케일을 읽어 판정한다. SQLite 는 로케일이 없어 늘 nil 이다.
// 거절이면 *LocaleError, 읽기 실패면 그 오류다(부르는 쪽이 둘을 errors.As 로 가른다).
func localeGate(ctx context.Context, q dbtx) error {
	if q.dialect() != enginePG {
		return nil
	}
	f, err := readLocale(ctx, q)
	if err != nil {
		return err
	}
	return JudgeLocale(f)
}

// CheckLocale 은 이 원장의 DB 로케일이 지금 관문을 지나는가다(/healthz 의 db_locale_ok). nil 이면 지난다.
// 거절은 *LocaleError 이고, 그 밖의 오류는 못 읽은 것이다(드라이버 오류는 store 경계에서 이미 옮겨졌다).
func (s *Store) CheckLocale(ctx context.Context) error { return localeGate(ctx, s.db) }

// IsLocaleRefusal 은 err 가 로케일 판정의 거절인가다(읽기 실패와 가른다).
func IsLocaleRefusal(err error) bool {
	var le *LocaleError
	return errors.As(err, &le)
}
