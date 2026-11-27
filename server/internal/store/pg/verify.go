package pg

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// 이관 대조의 순수한 몫이다: 행 하나의 지문, 표 하나의 대조, 순번 하나의 판정. 두 원장에서 같은 규칙으로 잰다 - 읽은 값은
// 둘 다 Cell 을 지나 같은 꼴(nil · int64 · string · []byte)이 된 뒤 여기에 온다.

// MaxListed 는 대조 · 사전 검사가 갈래마다 늘어놓는 행 좌표의 상한이다. 나머지는 수로만 낸다.
const MaxListed = 20

// RowHash 는 행 하나의 전 칸 지문이다. 칸을 선언 순서로 잇고, 칸마다 저장 타입 표지 한 바이트(NULL · 정수 · 실수 · 글 ·
// BLOB) 뒤에, NULL 이 아니면 길이(8바이트 빅엔디언) · 바이트를 둔다. 정수는 10진 글, 실수는 가장 짧은 10진 표기, 글과
// bytea 는 원문 바이트다 - 시각 · JSON 도 원문 그대로라 바뀐 자리 하나가 지문을 바꾼다. 길이를 앞에 두므로 칸 경계가 밀린
// 두 행(「ab」+「c」와 「a」+「bc」)이 같은 지문을 갖지 않고, 표지가 있으므로 NULL 과 빈 글이, 바이트가 같은 다른 타입(정수
// 7 · 글 '7' · BLOB X'37')이 갈린다.
//
// ★ 두 원장이 같은 규칙으로 잰다. 표지는 Cell 을 지난 값의 타입이다. Cell 은 사전 검사를 지나는 값을 칸 타입으로 옮기고
// (바이트가 그대로인 두 갈래 - 글 칸의 BLOB · bytea 칸의 TEXT - 뿐이다), 저장 타입이 칸과 달라 걸린 값은 원래 저장 타입을
// 그대로 둔다. Postgres 는 칸 타입으로 읽는다. 그래서 사전 검사를 지난 원본 행은 대상과 표지가 같고, 저장 타입이 칸과 다른
// 원본 행은 표지가 달라 어긋난다(import_test.go 의 TestCellKeepsTheColumnType 이 그 짝을 잠근다).
func RowHash(vals []any) [32]byte {
	h := sha256.New()
	var n [8]byte
	for _, v := range vals {
		tag, b := cellBytes(v)
		h.Write([]byte{tag})
		if tag == tagNull {
			continue
		}
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// 행 지문의 저장 타입 표지. SQLite 의 저장 갈래(storage class) 다섯과, Cell 을 지나지 않은 값 하나다.
const (
	tagNull byte = iota
	tagInt
	tagReal
	tagText
	tagBlob
	tagOther
)

func cellBytes(v any) (tag byte, b []byte) {
	switch x := v.(type) {
	case nil:
		return tagNull, nil
	case int64:
		return tagInt, strconv.AppendInt(nil, x, 10)
	case float64:
		return tagReal, strconv.AppendFloat(nil, x, 'g', -1, 64)
	case string:
		return tagText, []byte(x)
	case []byte:
		return tagBlob, x
	}
	// Cell 을 지나지 않은 값이다. 타입 이름을 지문에 넣어 어느 쪽과도 우연히 같지 않게 한다.
	return tagOther, fmt.Appendf(nil, "%T:%v", v, v)
}

// RowMark 는 대조가 행 하나에서 들고 있는 것이다: 지문과, 어긋났을 때 사람에게 보일 행 좌표.
type RowMark struct {
	Hash [32]byte
	Key  string
}

// Mark 는 표 t 의 행 하나(칸 선언 순서, Cell 을 지난 값)의 대조 표지다.
func (t Table) Mark(vals []any) RowMark {
	return RowMark{Hash: RowHash(vals), Key: t.KeyLabel(vals)}
}

// KeyLabel 은 행을 가리키는 칸(Column.Key)의 값을 「칸=값」으로 이은 좌표다. 글은 따옴표로 감싸 NUL · 줄바꿈이 보이게 한다.
func (t Table) KeyLabel(vals []any) string {
	var parts []string
	for i, c := range t.Cols {
		if !c.Key || i >= len(vals) {
			continue
		}
		parts = append(parts, c.Name+"="+labelValue(vals[i]))
	}
	return strings.Join(parts, " ")
}

func labelValue(v any) string {
	const max = 80
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case string:
		if len(x) > max {
			x = x[:max] + "…"
		}
		return strconv.Quote(x)
	case []byte:
		if len(x) > max {
			x = append(x[:max:max], "…"...)
		}
		return strconv.Quote(string(x))
	}
	return fmt.Sprint(v)
}

// TableCheck 는 표 하나의 대조 결과다.
type TableCheck struct {
	Table          string
	Source, Target int
	// SourceDigest · TargetDigest 는 표 지문이다: 행 지문을 바이트 순으로 정렬해 이은 sha256.
	SourceDigest, TargetDigest [32]byte
	// OnlySource · OnlyTarget 은 한쪽에만 있는 행(지문 기준, 중복 수까지 센다)의 좌표다. MaxListed 까지만 싣는다.
	OnlySource, OnlyTarget []string
	// MissingSource · MissingTarget 은 한쪽에만 있는 행의 전체 수다(OnlySource · OnlyTarget 은 그 앞쪽만 싣는다).
	MissingSource, MissingTarget int
}

// OK 는 두 원장의 이 표가 행 지문의 다중집합으로 같은가다.
func (c TableCheck) OK() bool {
	return c.Source == c.Target && c.SourceDigest == c.TargetDigest
}

// Compare 는 표 하나의 양쪽 행 표지를 맞댄다. 받은 슬라이스를 정렬한다(제자리).
//
// ★ 표 지문은 행 지문을 **정렬해서** 잇는다(기본키 순이 아니다). 기본키 순으로 이으려면 두 엔진이 같은 순서로 내야 하는데,
// 글 기본키의 순서는 콜레이션(SQLite BINARY 와 Postgres 의 DB 콜레이션)이 정하고 둘이 다르다. 정렬한 지문은 순서를 묻지
// 않는다. 그래도 잃는 것이 없다 - 행 지문이 전 칸이라 「같은 행이 다른 자리에 있다」는 기본키가 같은 다른 행이 된다.
// item_after 처럼 순서가 뜻인 표는 그 순서가 칸(seq = rowid)에 들어 있어 지문에 실린다.
func Compare(table string, src, dst []RowMark) TableCheck {
	byHash := func(a, b RowMark) int { return bytes.Compare(a.Hash[:], b.Hash[:]) }
	slices.SortFunc(src, byHash)
	slices.SortFunc(dst, byHash)
	c := TableCheck{Table: table, Source: len(src), Target: len(dst), SourceDigest: digest(src), TargetDigest: digest(dst)}
	i, j := 0, 0
	for i < len(src) || j < len(dst) {
		switch {
		case j == len(dst) || (i < len(src) && byHash(src[i], dst[j]) < 0):
			c.MissingTarget++
			if len(c.OnlySource) < MaxListed {
				c.OnlySource = append(c.OnlySource, src[i].Key)
			}
			i++
		case i == len(src) || byHash(src[i], dst[j]) > 0:
			c.MissingSource++
			if len(c.OnlyTarget) < MaxListed {
				c.OnlyTarget = append(c.OnlyTarget, dst[j].Key)
			}
			j++
		default:
			i++
			j++
		}
	}
	return c
}

func digest(marks []RowMark) [32]byte {
	h := sha256.New()
	for _, m := range marks {
		h.Write(m.Hash[:])
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// Sequence 는 identity 칸의 시퀀스를 다시 시작할 번호(`ALTER SEQUENCE … RESTART WITH`)다 - 다음 nextval 이 이 번호를 낸다.
// floor 는 원본에서 이미 발급된 가장 큰 번호다. AUTOINCREMENT 표는 greatest(max(칸), sqlite_sequence.seq)(지워진 최고 번호도
// 다시 내지 않는다), 아닌 표는 max(rowid). 발급된 번호가 없으면 floor 가 0 이라 1 부터 낸다(새 시퀀스와 같다).
func Sequence(floor int64) int64 { return floor + 1 }

// SequenceCheck 는 identity 하나의 순번 대조다.
type SequenceCheck struct {
	Table, Column string
	// Floor 는 원본에서 발급된 가장 큰 번호다(Sequence 의 floor).
	Floor int64
	// Next 는 대상 시퀀스가 다음에 낼 번호다(last_value, is_called 로 계산 - NextValue).
	Next int64
}

// OK 는 대상이 원본에서 발급된 어느 번호도 다시 내지 않는가다.
func (c SequenceCheck) OK() bool { return c.Next > c.Floor }

// NextValue 는 시퀀스의 last_value · is_called 에서 다음 nextval 이 낼 번호다(증분 1).
func NextValue(last int64, called bool) int64 {
	if called {
		return last + 1
	}
	return last
}
