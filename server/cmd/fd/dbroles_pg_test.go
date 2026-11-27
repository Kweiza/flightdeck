package main

// deploy/db-roles.sh 를 **그대로** 실제 Postgres(FD_TEST_PG 의 클러스터)에 돌리는 시험이다. 가짜 psql 시험
// (TestDBRolesKeepsPasswordsOutOfArgvAndEnv)은 psql 에 넘기는 모양만 잰다 - DO 블록 · regrole · pg_has_role(…, 'SET') ·
// psql 변수(:'x' · :"x" · 역따옴표 \set) 가 서버에서 실제로 도는지, 두 번 돌려도 같은지, 어긋난 계정에서 아무것도 안 바꾸고
// 서는지는 서버만 답한다.
//
// ★ 스크립트는 시험 PG 컨테이너(compose.test.yaml)와 **같은 이미지 · 같은 네트워크 이름공간**의 일회용 컨테이너에서 돈다
//   (`docker run --network container:<그 컨테이너>`). 운영의 db-roles 서비스와 같은 sh · psql 이고, 이 기계에 psql 이 없어도
//   돈다. 소켓 대신 127.0.0.1:5432 에 초사용자로 붙는다(시험 클러스터는 trust). FD_TEST_PG 의 포트를 내는 컨테이너가 없으면
//   **실패**한다 - 건너뛰면 아무것도 안 잰 채 초록이다.
// ★ 역할은 클러스터 전체의 것이라 시험마다 이름을 새로 짓고 끝나면 지운다. 다른 시험의 준비(bootstrap.sql)가 「fd_app 을 쓰는
//   계정이 fd_owner 로 바꿔 탄다」를 재므로, fd_owner 에 닿는 어긋남은 fd_app 의 구성원이 아닌 새 계정에만 만든다(스크립트가 그
//   계정에 주는 fd_app 은 되감기는 트랜잭션 안이라 남에게 안 보인다).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/kweiza/flightdeck/internal/store"
	"github.com/kweiza/flightdeck/internal/store/storetest"
)

// pgRolesRig 는 실제 클러스터에서 db-roles.sh 를 돌리는 자리다. 계정 한 벌(서버 · 덤프)과 그 pgpass 파일을 든다.
type pgRolesRig struct {
	t                 *testing.T
	admin             *sql.DB
	container, image  string
	db, dir           string
	srv, bak          string
	srvPass, bakPass  string
	srvFile, bakFile  string
	deployDir, script string
}

func newPGRolesRig(t *testing.T) *pgRolesRig {
	t.Helper()
	dsn := storetest.NewBare(t, "")
	// 묶음 역할 fd_owner · fd_app 은 `fd migrate` 의 준비가 만든다 - 배포에서도 db-roles 는 migrate 뒤에 돈다.
	if err := store.Migrate(context.Background(), dsn, quietLogger()); err != nil {
		t.Fatalf("준비: %v", err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	r := &pgRolesRig{t: t, admin: admin, db: strings.TrimPrefix(u.Path, "/"), dir: t.TempDir()}
	r.container, r.image = testPGContainer(t, u.Port())
	r.script = dbRolesScript(t)
	r.deployDir = filepath.Dir(r.script)
	r.newAccounts()
	return r
}

// testPGContainer 는 FD_TEST_PG 의 포트를 호스트에 내는 컨테이너와 그 이미지다.
func testPGContainer(t *testing.T, port string) (string, string) {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-q", "--filter", "publish="+port).Output()
	ids := strings.Fields(string(out))
	if err != nil || len(ids) != 1 {
		t.Fatalf("FD_TEST_PG 의 포트 %s 를 내는 컨테이너가 하나가 아니다(%v, %v) - 이 시험은 compose.test.yaml 의 pg-test 옆에서 "+
			"db-roles.sh 를 돌린다(AGENTS 의 검증 관문 한 줄)", port, ids, err)
	}
	img, err := exec.Command("docker", "inspect", "-f", "{{.Config.Image}}", ids[0]).Output()
	if err != nil {
		t.Fatalf("시험 PG 컨테이너의 이미지를 못 읽었다: %v", err)
	}
	return ids[0], strings.TrimSpace(string(img))
}

// newAccounts 는 새 계정 한 벌의 이름 · 비밀번호 · pgpass 파일을 정하고, 끝나면 그 역할을 지운다. 역할은 만들지 않는다.
func (r *pgRolesRig) newAccounts() {
	r.t.Helper()
	tag := randHex(r.t, 6)
	r.srv, r.bak = "fd_t_"+tag+"_srv", "fd_t_"+tag+"_bak"
	r.srvFile, r.bakFile = filepath.Join(r.dir, tag+"-server.pgpass"), filepath.Join(r.dir, tag+"-backup.pgpass")
	r.setPasswords("S-"+randHex(r.t, 12), "B-"+randHex(r.t, 12))
	srv, bak := r.srv, r.bak
	r.t.Cleanup(func() {
		if _, err := r.admin.Exec(`DROP ROLE IF EXISTS ` + srv + `, ` + bak); err != nil {
			r.t.Errorf("시험 역할을 못 지웠다: %v", err)
		}
	})
}

// setPasswords 는 두 pgpass 파일을 **제자리에서** 고쳐 쓴다(README 의 비밀번호 바꾸기와 같다).
func (r *pgRolesRig) setPasswords(srv, bak string) {
	r.t.Helper()
	r.srvPass, r.bakPass = srv, bak
	for f, line := range map[string]string{
		r.srvFile: "db:5432:" + r.db + ":" + r.srv + ":" + srv + "\n",
		r.bakFile: "db:5432:" + r.db + ":" + r.bak + ":" + bak + "\n",
	} {
		if err := os.WriteFile(f, []byte(line), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// run 은 스크립트를 그대로 돌린다 - 운영의 db-roles 서비스와 같은 환경 변수 이름, 붙는 곳만 소켓 대신 TCP 다.
func (r *pgRolesRig) run() (stdout, stderr string, rc int) {
	r.t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "--network", "container:"+r.container,
		"-v", r.deployDir+":/deploy:ro", "-v", r.dir+":/w:ro",
		"-e", "PGHOST=127.0.0.1", "-e", "PGPORT=5432", "-e", "PGUSER=postgres", "-e", "PGDATABASE="+r.db,
		"-e", "FD_PG_SERVER_USER="+r.srv, "-e", "FD_PG_BACKUP_USER="+r.bak,
		"-e", "FD_PG_SERVER_PGPASS=/w/"+filepath.Base(r.srvFile), "-e", "FD_PG_BACKUP_PGPASS=/w/"+filepath.Base(r.bakFile),
		"--entrypoint", "sh", r.image, "/deploy/"+filepath.Base(r.script))
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			r.t.Fatalf("db-roles 컨테이너를 못 돌렸다: %v", err)
		}
		rc = ee.ExitCode()
	}
	return out.String(), errb.String(), rc
}

func (r *pgRolesRig) exec(q string) {
	r.t.Helper()
	if _, err := r.admin.Exec(q); err != nil {
		r.t.Fatalf("%s: %v", q, err)
	}
}

// account 는 역할 하나의 모습이다: 있는가 · LOGIN · 특권 속성 · 구성원 자격(INHERIT · SET 째) · 비밀번호 검증자.
type account struct {
	exists, login, privileged bool
	memberOf                  []string
	verifier                  string
}

func (r *pgRolesRig) account(role string) account {
	r.t.Helper()
	var a account
	var verifier sql.NullString
	err := r.admin.QueryRow(`SELECT rolcanlogin, rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls,
		rolpassword FROM pg_authid WHERE rolname = $1`, role).Scan(&a.login, &a.privileged, &verifier)
	if err == sql.ErrNoRows {
		return a
	}
	if err != nil {
		r.t.Fatal(err)
	}
	a.exists, a.verifier = true, verifier.String
	rows, err := r.admin.Query(`SELECT m.roleid::regrole::text || ' inherit=' || m.inherit_option || ' set=' || m.set_option
		FROM pg_auth_members m WHERE m.member = $1::regrole ORDER BY 1`, role)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			r.t.Fatal(err)
		}
		a.memberOf = append(a.memberOf, s)
	}
	return a
}

// scramMatches 는 SCRAM-SHA-256 검증자(SCRAM-SHA-256$<반복>:<소금>$<StoredKey>:<ServerKey>)가 password 로 지은 것인가다
// (RFC 5802 · 7677). ASCII 비밀번호라 SASLprep 이 바꾸지 않는다.
func scramMatches(verifier, password string) bool {
	rest, ok := strings.CutPrefix(verifier, "SCRAM-SHA-256$")
	head, keys, ok2 := strings.Cut(rest, "$")
	iterS, saltS, ok3 := strings.Cut(head, ":")
	storedS, serverS, ok4 := strings.Cut(keys, ":")
	iter, err := strconv.Atoi(iterS)
	salt, err2 := base64.StdEncoding.DecodeString(saltS)
	if !ok || !ok2 || !ok3 || !ok4 || err != nil || err2 != nil {
		return false
	}
	salted, err := pbkdf2.Key(sha256.New, password, salt, iter, sha256.Size)
	if err != nil {
		return false
	}
	mac := func(msg string) []byte {
		h := hmac.New(sha256.New, salted)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	stored := sha256.Sum256(mac("Client Key"))
	return base64.StdEncoding.EncodeToString(stored[:]) == storedS && base64.StdEncoding.EncodeToString(mac("Server Key")) == serverS
}

// 맞추기: 계정 둘을 LOGIN · 특권 없이 만들고, 서버 계정은 fd_app 만, 덤프 계정은 pg_read_all_data 만의 구성원으로 두고,
// 비밀번호를 pgpass 의 값(SCRAM)으로 둔다. 두 번 돌려도 같고, pgpass 를 제자리에서 고쳐 다시 돌리면 비밀번호만 바뀐다.
func TestDBRolesOnARealCluster(t *testing.T) {
	r := newPGRolesRig(t)
	check := func(step string) {
		t.Helper()
		for role, want := range map[string]string{r.srv: "fd_app inherit=true set=true", r.bak: "pg_read_all_data inherit=true set=true"} {
			a := r.account(role)
			if !a.exists || !a.login || a.privileged || !slices.Equal(a.memberOf, []string{want}) {
				t.Fatalf("%s: %s 의 모습이 %+v 다 - LOGIN · 특권 없음 · %s 만이어야 한다", step, role, a, want)
			}
		}
		if !scramMatches(r.account(r.srv).verifier, r.srvPass) || !scramMatches(r.account(r.bak).verifier, r.bakPass) {
			t.Fatalf("%s: 비밀번호가 pgpass 의 값이 아니다", step)
		}
	}
	for i, step := range []string{"처음", "다시(같은 값)"} {
		out, stderr, rc := r.run()
		if rc != 0 || !strings.Contains(out, "맞췄다: "+r.srv+"(fd_app) · "+r.bak+"(pg_read_all_data)") {
			t.Fatalf("%s(%d): rc=%d\n%s\n%s", step, i, rc, out, stderr)
		}
		check(step)
	}
	r.setPasswords("S2-"+randHex(t, 12), "B2-"+randHex(t, 12))
	if out, stderr, rc := r.run(); rc != 0 {
		t.Fatalf("비밀번호 바꾸기: rc=%d\n%s\n%s", rc, out, stderr)
	}
	check("비밀번호 바꾸기")
}

// 어긋난 계정: 고치지 않고 1 로 서며 아무것도 바꾸지 않는다(만들기 · 구성원 자격 · 비밀번호가 한 트랜잭션이라 함께 되감긴다).
// 갈래마다 새 계정 한 벌에 어긋남을 미리 만든다. fd_owner · fd_app 에 SET 만 있고 INHERIT 가 없는 구성원 자격은 pg_has_role 의
// 'SET' 갈래만 잡는다('USAGE' 는 거짓이다).
func TestDBRolesStopsOnAMismatchedAccount(t *testing.T) {
	r := newPGRolesRig(t)
	var owner string
	if err := r.admin.QueryRow(`SELECT datdba::regrole::text FROM pg_database WHERE datname = $1`, r.db).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, setup, undo, want string
	}{
		{"특권 속성", `CREATE ROLE {srv} LOGIN CREATEDB`, ``, `{srv}: CREATEDB`},
		{"NOLOGIN", `CREATE ROLE {srv} NOLOGIN`, ``, `{srv}: NOLOGIN`},
		{"남는 구성원 자격", `CREATE ROLE {srv} LOGIN; GRANT pg_monitor TO {srv}`, ``, `{srv}: pg_monitor 의 구성원이다`},
		{"서버 계정이 fd_owner 로 바꿔 탄다(SET 만)", `CREATE ROLE {srv} LOGIN; GRANT fd_owner TO {srv} WITH INHERIT FALSE, SET TRUE`,
			``, `{srv}: fd_owner 로 바꿔 타거나 그 권한을 물려받는다`},
		{"덤프 계정이 fd_app 에 닿는다(SET 만)", `CREATE ROLE {bak} LOGIN; GRANT fd_app TO {bak} WITH INHERIT FALSE, SET TRUE`,
			``, `{bak}: fd_app 에 닿는다`},
		{"계정을 구성원으로 둔 역할", `CREATE ROLE {srv} LOGIN; CREATE ROLE {bak} LOGIN; GRANT {srv} TO {bak}`,
			``, `{srv} 를 {bak} 가 구성원으로 둔다`},
		{"DB 의 주인", `CREATE ROLE {srv} LOGIN; ALTER DATABASE {db} OWNER TO {srv}`, `ALTER DATABASE {db} OWNER TO {owner}`,
			`{srv}: DB 의 주인이다`},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := *r // 갈래마다 새 계정 한 벌 - 정리(DROP ROLE)는 이 갈래가 끝날 때, 되돌리기(undo) 뒤에 돈다
			r.t = t
			r.newAccounts()
			fill := strings.NewReplacer("{srv}", r.srv, "{bak}", r.bak, "{db}", r.db, "{owner}", owner).Replace
			for _, q := range strings.Split(fill(c.setup), "; ") {
				r.exec(q)
			}
			if c.undo != "" {
				defer r.exec(fill(c.undo))
			}
			before := map[string]account{r.srv: r.account(r.srv), r.bak: r.account(r.bak)}
			out, stderr, rc := r.run()
			if rc != 1 || !strings.Contains(stderr, "고치지 않고 멈춘다") || !strings.Contains(stderr, fill(c.want)) ||
				!strings.Contains(stderr, "아무것도 바뀌지 않았다") {
				t.Fatalf("rc=%d, %q 로 서지 않았다:\n%s\n%s", rc, fill(c.want), out, stderr)
			}
			for role, was := range before {
				if got := r.account(role); !slices.Equal(got.memberOf, was.memberOf) || got.exists != was.exists ||
					got.verifier != was.verifier || got.login != was.login {
					t.Errorf("섰다면서 %s 를 바꿨다: %+v → %+v", role, was, got)
				}
			}
		})
	}
}
