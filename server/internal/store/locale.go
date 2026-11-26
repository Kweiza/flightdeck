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

// minServerVersion 은 로케일 관문이 기대는 서버의 가장 낮은 판(server_version_num)이다. builtin 정렬 제공자와
// pg_database.datlocale 이 PG 17 부터 있다.
const minServerVersion = 170000

// LocaleFacts 는 Postgres 원장 DB 의 로케일 사실이다. readLocale 이 읽고 JudgeLocale 이 판정한다.
type LocaleFacts struct {
	// ServerVersion 은 server_version_num 이다. minServerVersion 보다 낮으면 나머지 칸은 읽지 않은 영값이다.
	ServerVersion int
	Provider      string // pg_database.datlocprovider: b(builtin) · c(libc) · i(icu)
	Locale        string // datlocale - builtin · icu 제공자의 기본 로케일. libc 면 빈 문자열
	Collate       string // datcollate - libc LC_COLLATE
	CType         string // datctype - libc LC_CTYPE. pg_trgm 은 정렬 제공자가 아니라 이것으로 글자를 가른다
	// TrgmReady 는 pg_trgm 의 show_trgm 이 이 DB 에 있는가다. 없으면 Trigrams 는 값이 아니다.
	TrgmReady bool
	Trigrams  int // cardinality(show_trgm(trigramProbe))
}

// LocaleError 는 로케일 관문의 거절이다. Reasons 는 걸린 축마다 하나다(하나 이상).
type LocaleError struct{ Reasons []string }

func (e *LocaleError) Error() string {
	return "DB 로케일이 이 서버가 기대는 값이 아니다 - " + strings.Join(e.Reasons, " · ") +
		" (DB 를 PG 17 이상의 initdb --locale-provider=builtin --builtin-locale=C.UTF-8 --lc-collate=C.UTF-8 --lc-ctype=C.UTF-8 -E UTF8 로 " +
		"만든 클러스터에 새로 만들고 옮겨라. 로케일은 만든 뒤에 못 바꾼다)"
}

// cCollations 는 코드포인트(바이트) 순으로 정렬하는 libc LC_COLLATE 다.
var cCollations = map[string]bool{"C": true, "POSIX": true, "C.UTF-8": true, "C.utf8": true}

// cUTF8CTypes 는 받는 libc LC_CTYPE 다: C.UTF-8 하나(플랫폼에 따라 C.utf8 로 적힌다).
var cUTF8CTypes = map[string]bool{"C.UTF-8": true, "C.utf8": true}

// JudgeLocale 은 로케일 사실이 이 서버가 기대는 값인지 판정한다. 순수 함수다. nil 이면 통과다.
//
// 기대하는 값(compose.test.yaml 의 initdb 인자가 만드는 DB):
//   - 제공자 builtin(b), 기본 로케일 C.UTF-8. 정렬이 코드포인트 순이라 SQLite 의 BINARY 와 ORDER BY 결과가 같고,
//     lower · ILIKE 의 대소문자 접기가 libc · ICU 판본을 안 탄다.
//   - LC_COLLATE 가 C 계열(코드포인트 순). builtin 이면 기본 정렬은 이것을 안 쓰지만, 정렬 규칙이 libc 로 새는 자리
//     (COLLATE "default" 가 아닌 libc 정렬)가 생겨도 순서가 같게 둔다.
//   - LC_CTYPE 가 C.UTF-8, 그리고 show_trgm(trigramProbe) 가 비지 않는다. pg_trgm 이 이것으로 글자를 가르므로 `C` 면
//     한글 trigram 이 0개, 한글 검색이 조용히 0건이 된다. 언어 로케일(en_US.UTF-8 · ko_KR.UTF-8 · tr_TR.UTF-8 등)은
//     받지 않는다 - 글자 가르기와 대소문자 접기가 언어 규칙(터키어의 I 와 ı 등)과 libc 판본을 타서, 같은 판단이 DB 마다
//     다른 trigram 을 낸다. 이름이 맞아도 그 기계의 libc 가 한글을 글자로 보는지는 모르니 trigram 도 실제로 잰다.
//
// ★ 서버 판을 먼저 본다. PG 17 미만이면 builtin 제공자가 없어 나머지 축은 읽지도 않는다(readLocale) - 「PG 17 이상
// 필요」 하나만 낸다. 제공자 축의 거절로 내면 사람은 DB 를 다시 만들다 같은 판의 initdb 에서 막힌다.
//
// ★ 걸린 축을 다 모아 낸다. 처음 하나에서 멈추면 고친 뒤 다음 축에서 또 멈춘다 - 로케일은 DB 를 새로 만들어야
// 고쳐지므로 한 번에 다 보여야 한다.
func JudgeLocale(f LocaleFacts) error {
	if f.ServerVersion < minServerVersion {
		return &LocaleError{Reasons: []string{fmt.Sprintf("PG 17 이상 필요 - 서버가 %d.%d 다(server_version_num=%d). "+
			"이 서버가 기대는 builtin 정렬 제공자(C.UTF-8)는 PG 17 부터 있다", f.ServerVersion/10000, f.ServerVersion%10000, f.ServerVersion)}}
	}
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
	if !cUTF8CTypes[f.CType] {
		bad = append(bad, fmt.Sprintf("LC_CTYPE 가 C.UTF-8 이 아니다(datctype=%q) - pg_trgm 의 글자 가르기가 언어 규칙과 libc 판본을 탄다", f.CType))
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

// readLocale 은 지금 붙은 DB 의 로케일 사실을 읽는다. 판정은 안 한다(JudgeLocale).
//
// ★ 서버 판을 먼저 묻는다. datlocale 칸은 PG 17 부터 있어, 그 아래에서는 로케일 질의가 해석 단계에서 실패하고(42703)
// 「로케일을 읽지 못했다」는 읽기 실패로 보인다. 판이 낮으면 거기서 멈추고 판정(JudgeLocale)이 「PG 17 이상 필요」로 낸다.
//
// ★ show_trgm 은 따로 묻는다. 확장이 없으면 그 함수 이름이 든 문장은 해석 단계에서 실패하고(42883), 트랜잭션 안이면
// 그 트랜잭션을 버린다. 있는지 먼저 물어 사실(TrgmReady)로 낸다.
func readLocale(ctx context.Context, q Reader) (LocaleFacts, error) {
	var f LocaleFacts
	if err := q.QueryRowContext(ctx, `SELECT current_setting('server_version_num')::int`).Scan(&f.ServerVersion); err != nil {
		return f, fmt.Errorf("서버 판을 읽지 못했다: %w", err)
	}
	if f.ServerVersion < minServerVersion {
		return f, nil
	}
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
