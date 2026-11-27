package pg

// 역이관(`fd migrate export-sqlite` - Postgres 원장을 같은 판의 새 SQLite 파일로)에만 있는 순수한 몫이다. 표 목록 · 칸 ·
// 값 검사 · 행 지문 · 순번 규칙은 이관과 같은 것(import.go · verify.go)을 거꾸로 쓴다. 실행은 store 의 exportsqlite.go 다.

// Sidecars 는 SQLite 원장 path 옆에 붙는 파일들이다(-wal · -shm · 되감기 저널). 역이관은 이 중 하나라도 있는 자리에 새
// 원장을 내지 않는다 - 남은 -wal 은 새 파일을 처음 여는 연결이 자기 것으로 알고 되살려 덮는다.
func Sidecars(path string) []string {
	return []string{path + "-wal", path + "-shm", path + "-journal"}
}

// SearchRebuild 는 SQLite 판 판단 전문 검색 색인(judgment_fts - judgment 를 content 로 둔 FTS5)을 judgment 의 지금 행으로
// 다시 짓는 문장이다. 옮긴 judgment 행은 넣는 트리거(judgment_fts_ins)가 이미 색인하지만, 역이관은 색인을 트리거에 기대지
// 않고 원문에서 다시 지은 뒤 SearchCheck 로 잰다 - Postgres 에는 이 색인이 없어 옮길 원본이 없다.
const SearchRebuild = `INSERT INTO judgment_fts(judgment_fts) VALUES('rebuild')`

// SearchCheck 는 judgment_fts 색인이 content 표(judgment)와 맞는지 재는 문장이다. rank 1 이면 색인 자체의 정합만이 아니라
// content 표의 행과 대조하고, 어긋나면 SQLITE_CORRUPT_VTAB 로 실패한다.
const SearchCheck = `INSERT INTO judgment_fts(judgment_fts, rank) VALUES('integrity-check', 1)`
