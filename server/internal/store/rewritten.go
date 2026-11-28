package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/kweiza/flightdeck/internal/store/pg"
)

// 이관이 바꾼 칸의 기록: SQLite 원본의 NUL 을 `\x00` 표기로 바꿔 넣은 칸마다 한 줄(JSON Lines)을 원본 옆 파일
// <원본>.rewritten-<시각>.jsonl 에 남긴다. 그 칸의 원래 바이트는 이 파일이 정본이다 - 옮긴 원장의 값만으로는 바꾼 칸과 원래
// `\x00` 네 글자를 담은 칸이 갈리지 않는다(NUL 이 없는 칸 · 글은 그대로 옮긴다). 칸 뜻은 README 「SQLite 원장을 Postgres 로
// 옮기기」가 사람에게 적는다.
//
// ★ 커밋 전에 남긴다(transferInto 의 seal) - 못 남기면 커밋하지 않는다. 화면 보고(TransferReport.Rewrites)는 갈래마다 행 좌표를
//   pg.MaxListed 개만 싣지만 이 파일은 상한이 없다.
// ★ 다 쓰고 굳힌 임시 파일에 이름을 붙인다(하드 링크 - 있는 이름은 덮지 않고 번호를 붙인다, freshName). 이름이 붙은 파일은 다
//   쓴 것이다.

// rewrittenSuffix 는 기록 파일 이름에서 원본 이름 뒤에 붙는 꼬리다(<원본>.rewritten-<시각>.jsonl).
const rewrittenSuffix = ".rewritten"

// rewrittenCell 은 바꾼 칸 하나의 기록(기록 파일의 한 줄)이다.
type rewrittenCell struct {
	Table string `json:"table"`
	// Key 는 그 행의 Key 칸(pg.Column.Key) → 옮긴 값이다. 옮긴 원장에서 그 행을 찾는 값이고 자르지 않는다. 비밀 키 칸
	// (pg.Column.Secret - web_session.id_hash · device_code.code_hash)은 원값 대신 지문(pg.SecretMark)이다 - 이 파일은 원본 옆에
	// 남는다. 그 행은 지문으로 찾는다: Postgres 에서 `left(encode(sha256(convert_to(id_hash, 'UTF8')), 'hex'), 12)` 가 지문의
	// 「sha256:」 뒤와 같은 행이다. 다른 정보는 더 싣지 않는다 - 지문은 48비트라 그 표의 행 수로는 겹치지 않고, 겹쳐도 바꾼 칸
	// (Column)의 옮긴 값이 Original 을 바꿔 넣은 글이라 둘이 갈린다. 비밀 칸 자체는 바꿔 넣지 않으므로(pg.FaultSecretNUL)
	// Original 에 비밀이 실리는 일은 없다.
	Key    map[string]any `json:"key"`
	Column string         `json:"column"`
	Rule   pg.Rewrite     `json:"rule"`
	// Paths 는 JSON 칸에서 고친 글 값의 자리다(RFC 6901 JSON Pointer, 원래 문서의 키). 키는 고치지 않는다 - U+0000 이 든 키는
	// 사전 검사가 거절한다. 글 칸은 칸 전체가 단위라 비었다.
	Paths []string `json:"paths,omitempty"`
	// Original 은 원본에서 읽은 그 칸의 바이트다(JSON 에서는 base64).
	Original []byte `json:"original"`
}

// newRewrittenCell 은 표 t 의 행 r 에서 바꿔 넣은 칸 i 의 기록이다.
func newRewrittenCell(t pg.Table, r cellRow, i int) rewrittenCell {
	key := map[string]any{}
	for k, c := range t.Cols {
		switch {
		case c.Key && c.Secret:
			key[c.Name] = pg.SecretMark(r.vals[k])
		case c.Key:
			key[c.Name] = r.vals[k]
		}
	}
	var orig []byte
	switch x := r.read[i].(type) {
	case string:
		orig = []byte(x)
	case []byte:
		orig = slices.Clone(x)
	}
	c := t.Cols[i]
	return rewrittenCell{Table: t.Name, Key: key, Column: c.Name, Rule: r.rewrites[i], Paths: pg.RewritePaths(c, r.read[i]),
		Original: orig}
}

// linkRecord 는 기록 파일에 이름을 붙이는 하드 링크다(시험이 실패를 넣는 자리).
var linkRecord = os.Link

// writeRewriteRecord 는 SQLite 원본 from 옆에 바꾼 칸 cells 의 기록 파일을 남기고 그 이름을 낸다. 바꾼 칸이 없으면 아무것도 안
// 남기고 빈 이름이다. 이름은 at(이관을 시작한 때)으로 짓는다. 이름을 붙인 뒤 디렉토리를 못 굳히면 붙인 이름과 오류를 낸다.
func writeRewriteRecord(from string, cells []rewrittenCell, at time.Time) (string, error) {
	if len(cells) == 0 {
		return "", nil
	}
	dir := filepath.Dir(from)
	f, err := createExclusive(dir, "."+filepath.Base(from)+rewrittenSuffix+"-", ledgerFileMode)
	if err != nil {
		return "", fmt.Errorf("원본 디렉토리(%s)에 기록 파일을 만들지 못했다: %w", clip(dir, 200), err)
	}
	tmp := f.Name()
	// 이름을 붙였으면 그 이름이 같은 파일을 가리킨다(하드 링크). 임시 이름을 못 떼면 숨은 파일 하나가 남을 뿐이다.
	defer os.Remove(tmp)
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, c := range cells {
		if err = enc.Encode(c); err != nil {
			break
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("기록 파일(%s)에 쓰지 못했다: %w", clip(tmp, 200), err)
	}
	name, err := freshName(from+rewrittenSuffix+"-"+fileStamp(at), ".jsonl", func(n string) error { return linkRecord(tmp, n) })
	if err != nil {
		return "", fmt.Errorf("기록 파일에 이름을 붙이지 못했다: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return name, fmt.Errorf("기록 파일 %s 에 붙인 이름을 굳히지 못했다(디렉토리 fsync): %w", clip(name, 200), err)
	}
	return name, nil
}
