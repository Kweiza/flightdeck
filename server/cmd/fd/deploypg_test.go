package main

// 배포 정의(compose.yaml · compose.postgres.yaml · server/Dockerfile · deploy/db-roles.sh)의 시험.
//
// ★ compose 는 **글자가 아니라 렌더한 설정**으로 잰다(`docker compose config --format json`). 덮개 파일은 서비스를 합치므로
//   (depends_on 은 맵째 합쳐지고, command 는 갈아 끼워지고, volumes 는 대상 경로로 합쳐진다) 파일 하나의 글자로는 실제로
//   도는 모양이 안 보인다. 이 저장소가 이미 한 번 밟은 자리다 - 글자만 보던 시험이 한 번도 안 돈 command 를 못박고 있었다
//   (TestComposeCommandsDoNotRepeatTheEntrypoint 머리말). docker CLI(compose 플러그인)가 없으면 **실패**한다 - 건너뛰면 아무것도
//   안 잰 채 초록이다. 렌더는 도커 데몬에 붙지 않는다.

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type composeModel struct {
	Services map[string]composeSvc `json:"services"`
	Volumes  map[string]struct {
		Name     string `json:"name"`
		External bool   `json:"external"`
	} `json:"volumes"`
	Secrets map[string]struct {
		File string `json:"file"`
	} `json:"secrets"`
}

type composeSvc struct {
	Image         string             `json:"image"`
	ContainerName string             `json:"container_name"`
	Command       []string           `json:"command"`
	Entrypoint    []string           `json:"entrypoint"`
	Restart       string             `json:"restart"`
	User          string             `json:"user"`
	Profiles      []string           `json:"profiles"`
	Environment   map[string]*string `json:"environment"`
	DependsOn     map[string]struct {
		Condition string `json:"condition"`
		Restart   bool   `json:"restart"`
	} `json:"depends_on"`
	Volumes []struct {
		Type     string `json:"type"`
		Source   string `json:"source"`
		Target   string `json:"target"`
		ReadOnly bool   `json:"read_only"`
	} `json:"volumes"`
	Secrets []struct {
		Source string `json:"source"`
		Target string `json:"target"`
	} `json:"secrets"`
	Ports []json.RawMessage `json:"ports"`
}

func (s composeSvc) env(k string) (string, bool) {
	v, ok := s.Environment[k]
	if !ok || v == nil {
		return "", false
	}
	return *v, true
}

// mount 는 컨테이너 쪽 경로 target 에 붙은 것의 원천이다(없으면 ok=false).
func (s composeSvc) mount(target string) (source string, readOnly, ok bool) {
	for _, v := range s.Volumes {
		if v.Target == target {
			return v.Source, v.ReadOnly, true
		}
	}
	return "", false, false
}

func (s composeSvc) secretNames() []string {
	var out []string
	for _, x := range s.Secrets {
		out = append(out, x.Source)
	}
	sort.Strings(out)
	return out
}

func (m composeModel) serviceNames() []string {
	var out []string
	for n := range m.Services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// deployHome 은 렌더에 주는 가짜 홈이다. `~` 와 ${HOME} 기본값이 이 자리로 펼쳐져, 시험이 이 머신의 홈을 안 본다.
const deployHome = "/home/fdtest"

// renderCompose 는 저장소 루트의 compose 파일들을 env 만 준 채 렌더한다. 이 머신의 FD_* · COMPOSE_* · PG* 환경과 루트의
// .env 는 읽지 않는다(빈 --env-file 을 준다) - 개발자의 설정이 시험의 답을 바꾸면 안 된다.
func renderCompose(t *testing.T, env map[string]string, extraArgs []string, files ...string) (composeModel, string, error) {
	t.Helper()
	root := pluginRoot(t)
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker CLI 가 없다 - 배포 정의의 시험은 compose 가 렌더한 설정으로 잰다(건너뛰지 않는다): %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.env")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"compose", "--env-file", empty, "--project-directory", root, "-p", "fdtest-deploy"}
	for _, f := range files {
		args = append(args, "-f", filepath.Join(root, f))
	}
	args = append(args, extraArgs...)
	args = append(args, "config", "--format", "json")
	cmd := exec.Command("docker", args...)
	var keep []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "FD_") || strings.HasPrefix(k, "COMPOSE_") || strings.HasPrefix(k, "PG") || k == "HOME" {
			continue
		}
		keep = append(keep, kv)
	}
	// docker CLI 는 compose 플러그인을 설정 자리($HOME/.docker)에서 찾는다 - 홈을 바꾸기 전에 그 자리를 고정한다.
	if os.Getenv("DOCKER_CONFIG") == "" {
		if h, err := os.UserHomeDir(); err == nil {
			keep = append(keep, "DOCKER_CONFIG="+filepath.Join(h, ".docker"))
		}
	}
	keep = append(keep, "HOME="+deployHome)
	for k, v := range env {
		keep = append(keep, k+"="+v)
	}
	cmd.Env = keep
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return composeModel{}, errb.String(), err
	}
	var m composeModel
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("compose config 의 JSON 을 못 읽었다: %v\n%s", err, out.String())
	}
	return m, errb.String(), nil
}

func mustRender(t *testing.T, env map[string]string, extraArgs []string, files ...string) composeModel {
	t.Helper()
	m, stderr, err := renderCompose(t, env, extraArgs, files...)
	if err != nil {
		t.Fatalf("compose config(%v) 가 실패했다: %v\n%s", files, err, stderr)
	}
	return m
}

// pgLayerEnv 는 Postgres 겹을 렌더할 때 운영자가 주는 값이다(시크릿 두 자리는 필수).
func pgLayerEnv() map[string]string {
	return map[string]string{
		"FD_PG_SERVER_PGPASS_HOST": "/etc/fdtest/server.pgpass",
		"FD_PG_BACKUP_PGPASS_HOST": "/etc/fdtest/backup.pgpass",
		"FD_DUMP_TIMEOUT":          "45m",
		"FD_UID":                   "1234",
		"FD_GID":                   "5678",
	}
}

func renderPG(t *testing.T) composeModel {
	t.Helper()
	return mustRender(t, pgLayerEnv(), nil, "compose.yaml", "compose.postgres.yaml")
}

// TestComposeDefaultPathStaysSQLite 는 덮개 파일을 안 얹은 기본 `up` 이 SQLite 파일 원장 그대로인지 잰다.
//
// ★ 이 배포의 첫 약속이다: 갱신 한 번에 서버가 말없이 다른(빈) 원장을 보면 안 된다. Postgres 겹의 변수(DB 이름 · 계정 ·
//
//	시크릿 자리)를 다 줘도, profile 을 켜도 기본 렌더에는 그 겹의 서비스 · 시크릿 · 볼륨 · 접속이 하나도 없어야 한다.
func TestComposeDefaultPathStaysSQLite(t *testing.T) {
	env := pgLayerEnv()
	env["FD_PG_DATABASE"] = "other"
	env["FD_PG_SERVER_USER"] = "someone"
	for _, extra := range [][]string{nil, {"--profile", "postgres"}} {
		m := mustRender(t, env, extra, "compose.yaml")
		if got := m.serviceNames(); !slices.Equal(got, []string{"flightdeck", "migrate"}) {
			t.Fatalf("기본 compose(%v)의 서비스가 %v 다 - migrate · flightdeck 둘이어야 한다", extra, got)
		}
		if len(m.Secrets) != 0 || len(m.Volumes) != 0 {
			t.Fatalf("기본 compose(%v)에 시크릿 %v · 볼륨 %v 가 있다 - SQLite 배치는 시크릿 0 · 이름 볼륨 0 이다", extra, m.Secrets, m.Volumes)
		}
		mig, srv := m.Services["migrate"], m.Services["flightdeck"]
		if !slices.Equal(mig.Command, []string{"migrate", "--db", "/data/fd.db"}) {
			t.Errorf("기본 migrate 의 command 가 %q 다 - SQLite 파일을 올려야 한다", mig.Command)
		}
		if v, _ := srv.env("FD_DB"); v != "/data/fd.db" {
			t.Errorf("기본 서버의 FD_DB 가 %q 다 - /data/fd.db 여야 한다", v)
		}
		if src, _, ok := srv.mount("/data"); !ok || src != deployHome+"/.flightdeck" {
			t.Errorf("기본 서버의 /data 가 %q 다 - 홈의 .flightdeck 이어야 한다", src)
		}
		for name, s := range m.Services {
			if len(s.Secrets) > 0 {
				t.Errorf("기본 %s 에 시크릿 %v 가 붙었다", name, s.secretNames())
			}
			for _, k := range []string{"PGPASSFILE", "PGHOST", "FD_BACKUP_DIR"} {
				if _, ok := s.env(k); ok {
					t.Errorf("기본 %s 에 Postgres 겹의 %s 가 있다", name, k)
				}
			}
			if _, ok := s.DependsOn["db"]; ok {
				t.Errorf("기본 %s 가 db 에 기댄다", name)
			}
		}
	}
}

// TestComposePostgresLayerWiring 은 compose.postgres.yaml 을 얹은 렌더가 배포의 약속대로 묶였는지 잰다.
//
// 재는 것: 서비스 다섯과 순서(db → migrate → db-roles → 서버 · 매일 덤프), 원장 URL, 누가 무엇을 붙이나(소켓 볼륨은 관리
// 컨테이너만, 덤프 자리는 migrate 와 db-backup 만, 시크릿은 쓰는 쪽만), 매일 덤프가 migrate 를 다시 만들 때 멈추는 것(restart),
// 고정 이름 external 볼륨, 비밀번호가 환경 변수 · URL 에 없는 것.
func TestComposePostgresLayerWiring(t *testing.T) {
	m := renderPG(t)
	if got := m.serviceNames(); !slices.Equal(got, []string{"db", "db-backup", "db-roles", "flightdeck", "migrate"}) {
		t.Fatalf("Postgres 겹의 서비스가 %v 다", got)
	}
	db, mig, roles, srv, bak := m.Services["db"], m.Services["migrate"], m.Services["db-roles"], m.Services["flightdeck"], m.Services["db-backup"]

	// 원장 DB.
	if db.ContainerName != "flightdeck-db" {
		t.Errorf("db 의 container_name 이 %q 다 - 고정해야 판이 다른 두 디렉토리가 한 데이터 볼륨에 postmaster 둘을 못 띄운다", db.ContainerName)
	}
	if len(db.Ports) != 0 {
		t.Errorf("db 가 포트를 낸다(%s) - compose 망 밖에 원장을 열지 않는다", db.Ports)
	}
	if src, _, ok := db.mount("/var/lib/postgresql/data"); !ok || src != "pgdata" {
		t.Errorf("db 의 데이터 자리가 %q 다 - pgdata 볼륨이어야 한다", src)
	}
	for _, name := range []string{"pgdata", "pgrun"} {
		v, ok := m.Volumes[name]
		if !ok || !v.External || v.Name != "flightdeck-"+name {
			t.Errorf("볼륨 %s 가 %+v 다 - 이름 flightdeck-%s 의 external 이어야 한다(판마다 디렉토리가 달라도 같은 원장 · `down -v` 가 못 지운다)", name, v, name)
		}
	}
	if v, _ := db.env("POSTGRES_PASSWORD_FILE"); v != "/proc/sys/kernel/random/uuid" {
		t.Errorf("db 의 POSTGRES_PASSWORD_FILE 이 %q 다 - 초사용자는 아무도 모르는 난수여야 한다(네트워크로 못 붙는다)", v)
	}
	if v, _ := db.env("POSTGRES_DB"); v != "flightdeck" {
		t.Errorf("db 의 POSTGRES_DB 가 %q 다", v)
	}

	// 관리 컨테이너(migrate): 초사용자 · 소켓 · 덤프 자리.
	if !slices.Equal(mig.Command, []string{"migrate"}) {
		t.Errorf("Postgres 겹 migrate 의 command 가 %q 다 - `--db /data/fd.db` 가 남으면 SQLite 파일을 올린다", mig.Command)
	}
	if d := mig.DependsOn["db"]; d.Condition != "service_healthy" {
		t.Errorf("migrate 가 db 를 %q 로 기다린다 - service_healthy 여야 한다", d.Condition)
	}
	if v, _ := mig.env("FD_DB"); v != "postgres://postgres@/flightdeck?host=/var/run/postgresql" {
		t.Errorf("migrate 의 FD_DB 가 %q 다 - 초사용자로 소켓에 붙어야 한다(빈 DB 의 준비는 초사용자만 한다)", v)
	}
	if v, _ := mig.env("FD_BACKUP_DIR"); v != "/backups" {
		t.Errorf("migrate 의 FD_BACKUP_DIR 이 %q 다", v)
	}
	for k, want := range map[string]string{"PGHOST": "/var/run/postgresql", "PGPORT": "5432", "PGUSER": "postgres", "PGDATABASE": "flightdeck"} {
		if v, _ := mig.env(k); v != want {
			t.Errorf("migrate 의 %s 가 %q 다 - %q 여야 한다(psql · pg_restore · 되돌리기 스크립트가 읽는다)", k, v, want)
		}
	}
	migBackups, _, ok := mig.mount("/backups")
	if !ok || migBackups != deployHome+"/.flightdeck-backups" {
		t.Errorf("migrate 의 /backups 가 %q 다", migBackups)
	}

	// 접속 계정 one-shot.
	if d := roles.DependsOn["migrate"]; d.Condition != "service_completed_successfully" {
		t.Errorf("db-roles 가 migrate 를 %q 로 기다린다 - 묶음 역할 fd_app 은 migrate 의 준비가 만든다", d.Condition)
	}
	if !slices.Equal(roles.Entrypoint, []string{"sh", "/usr/local/bin/fd-db-roles"}) || roles.Restart != "no" {
		t.Errorf("db-roles 의 entrypoint · restart 가 %q · %q 다", roles.Entrypoint, roles.Restart)
	}
	if src, ro, ok := roles.mount("/usr/local/bin/fd-db-roles"); !ok || !ro || src != filepath.Join(pluginRoot(t), "deploy", "db-roles.sh") {
		t.Errorf("db-roles 의 스크립트 마운트가 %q(ro=%v) 다", src, ro)
	}
	if got := roles.secretNames(); !slices.Equal(got, []string{"fd_pg_backup_pgpass", "fd_pg_server_pgpass"}) {
		t.Errorf("db-roles 의 시크릿이 %v 다 - 두 계정의 pgpass 둘이어야 한다", got)
	}
	for k, want := range map[string]string{
		"FD_PG_SERVER_USER": "fd_server", "FD_PG_BACKUP_USER": "fd_backup", "PGHOST": "/var/run/postgresql", "PGUSER": "postgres",
		"FD_PG_SERVER_PGPASS": "/run/secrets/fd_pg_server_pgpass", "FD_PG_BACKUP_PGPASS": "/run/secrets/fd_pg_backup_pgpass",
	} {
		if v, _ := roles.env(k); v != want {
			t.Errorf("db-roles 의 %s 가 %q 다 - %q 여야 한다", k, v, want)
		}
	}

	// 서버: 앱 계정 · TCP · 시크릿 하나.
	for dep, cond := range map[string]string{"db": "service_healthy", "db-roles": "service_completed_successfully", "migrate": "service_completed_successfully"} {
		if d := srv.DependsOn[dep]; d.Condition != cond {
			t.Errorf("서버가 %s 를 %q 로 기다린다 - %q 여야 한다", dep, d.Condition, cond)
		}
	}
	dsn, _ := srv.env("FD_DB")
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "postgres" || u.Host != "db:5432" || u.Path != "/flightdeck" || u.User.Username() != "fd_server" {
		t.Errorf("서버의 FD_DB 가 %q 다 - postgres://fd_server@db:5432/flightdeck 여야 한다", dsn)
	} else if _, has := u.User.Password(); has || u.RawQuery != "" {
		t.Errorf("서버의 FD_DB(%q)에 비밀번호나 질의가 있다 - 비밀번호는 pgpass 파일로만 넘긴다(docker inspect 에 보인다)", dsn)
	}
	if v, _ := srv.env("PGPASSFILE"); v != "/run/secrets/fd_pg_server_pgpass" {
		t.Errorf("서버의 PGPASSFILE 이 %q 다", v)
	}
	if got := srv.secretNames(); !slices.Equal(got, []string{"fd_pg_server_pgpass"}) {
		t.Errorf("서버의 시크릿이 %v 다 - 서버 계정의 pgpass 하나여야 한다(덤프 계정의 것을 서버가 읽으면 안 된다)", got)
	}

	// 매일 덤프.
	if d := bak.DependsOn["migrate"]; d.Condition != "service_completed_successfully" || !d.Restart {
		t.Errorf("db-backup 의 migrate 의존이 %+v 다 - 성공을 기다리고(restart: true) migrate 를 다시 만들 때 먼저 멈춰야 판올림의 DDL 이 덤프의 잠금에 안 걸린다", d)
	}
	if d := bak.DependsOn["db-roles"]; d.Condition != "service_completed_successfully" {
		t.Errorf("db-backup 이 db-roles 를 %q 로 기다린다 - 덤프 계정은 db-roles 가 만든다", d.Condition)
	}
	if len(bak.Profiles) != 0 {
		t.Errorf("db-backup 이 profile %v 뒤에 있다 - Postgres 겹을 얹으면 늘 떠야 한다", bak.Profiles)
	}
	for k, want := range map[string]string{
		"PGHOST": "db", "PGPORT": "5432", "PGDATABASE": "flightdeck", "PGUSER": "fd_backup",
		"PGPASSFILE": "/run/secrets/fd_pg_backup_pgpass", "FD_DB_BACKUP_DIR": "/backups/daily", "FD_DB_BACKUP_KEEP": "14",
		dumpTimeoutEnv: "45m",
	} {
		if v, _ := bak.env(k); v != want {
			t.Errorf("db-backup 의 %s 가 %q 다 - %q 여야 한다", k, v, want)
		}
	}
	if got := bak.secretNames(); !slices.Equal(got, []string{"fd_pg_backup_pgpass"}) {
		t.Errorf("db-backup 의 시크릿이 %v 다 - 덤프 계정의 pgpass 하나여야 한다", got)
	}
	if src, ro, ok := bak.mount("/usr/local/bin/fd-db-backup"); !ok || !ro || src != filepath.Join(pluginRoot(t), "deploy", "db-backup.sh") {
		t.Errorf("db-backup 의 스크립트 마운트가 %q(ro=%v) 다", src, ro)
	}
	if src, _, _ := bak.mount("/backups"); src != migBackups {
		t.Errorf("db-backup 의 /backups(%q)가 migrate 의 것(%q)과 다르다 - 판올림 전 덤프와 매일 덤프는 한 자리의 뿌리 · 하위다", src, migBackups)
	}
	// 판올림 전 덤프와 매일 덤프는 기한 설정 하나를 나눈다.
	if v, _ := mig.env(dumpTimeoutEnv); v != "45m" {
		t.Errorf("migrate 의 %s 가 %q 다 - db-backup 과 같은 값이어야 한다", dumpTimeoutEnv, v)
	}

	// 누가 무엇을 붙이나.
	for name, s := range m.Services {
		_, _, socket := s.mount("/var/run/postgresql")
		if want := name == "db" || name == "migrate" || name == "db-roles"; socket != want {
			t.Errorf("%s 의 소켓 볼륨 마운트가 %v 다 - 소켓(초사용자 trust)은 db · migrate · db-roles 만 붙인다", name, socket)
		}
		if _, _, b := s.mount("/backups"); b != (name == "migrate" || name == "db-backup") {
			t.Errorf("%s 의 /backups 마운트가 %v 다 - 덤프(원장 전체)는 migrate · db-backup 만 본다", name, b)
		}
		for _, leak := range []string{"PGPASSWORD", "POSTGRES_PASSWORD", "POSTGRES_HOST_AUTH_METHOD"} {
			if _, ok := s.env(leak); ok {
				t.Errorf("%s 에 %s 가 있다 - 비밀번호 · 인증 방식을 환경 변수로 넘기지 않는다", name, leak)
			}
		}
		if name != "db" && s.User != "1234:5678" {
			t.Errorf("%s 의 user 가 %q 다 - 모두 FD_UID:FD_GID 로 돈다(덤프 · 원장 파일의 주인)", name, s.User)
		}
	}
	for name, want := range map[string]string{"fd_pg_server_pgpass": "/etc/fdtest/server.pgpass", "fd_pg_backup_pgpass": "/etc/fdtest/backup.pgpass"} {
		if got := m.Secrets[name].File; got != want {
			t.Errorf("시크릿 %s 의 파일이 %q 다 - %q 여야 한다", name, got, want)
		}
	}
}

// TestComposePostgresImagesMatchTheTestServer 는 원장 DB · 계정 · 덤프의 postgres 이미지와 initdb 인자가 시험 DB(compose.test.yaml
// 의 pg-test)와 같은지 잰다. 원장의 주판을 올리는 사람이 시험 DB 만 올리면(또는 그 반대) pg_dump 가 더 높은 서버를 거절해 매일
// 덤프가 조용히 멈추고, 로케일 인자가 갈리면 시험이 잰 한글 검색이 운영 DB 에서 0건이 된다 - 그 자리가 여기서 빨갛다.
func TestComposePostgresImagesMatchTheTestServer(t *testing.T) {
	m := renderPG(t)
	test := mustRender(t, nil, nil, "compose.test.yaml").Services["pg-test"]
	if !strings.HasPrefix(test.Image, "postgres:") {
		t.Fatalf("pg-test 의 이미지가 %q 다", test.Image)
	}
	for _, name := range []string{"db", "db-roles", "db-backup"} {
		if got := m.Services[name].Image; got != test.Image {
			t.Errorf("%s 의 이미지(%s)가 시험 DB 의 이미지(%s)와 다르다 - 같은 주판이어야 한다", name, got, test.Image)
		}
	}
	want, _ := test.env("POSTGRES_INITDB_ARGS")
	got, _ := m.Services["db"].env("POSTGRES_INITDB_ARGS")
	if want == "" || !strings.HasPrefix(got, want+" ") {
		t.Fatalf("db 의 POSTGRES_INITDB_ARGS(%q)가 pg-test 의 인자(%q)로 시작하지 않는다 - 로케일 인자를 시험 DB 와 맞춘다", got, want)
	}
	for _, arg := range []string{"--lc-ctype=C.UTF-8", "--auth-local=trust", "--auth-host=scram-sha-256"} {
		if !slices.Contains(strings.Fields(got), arg) {
			t.Errorf("db 의 initdb 인자에 %s 가 없다", arg)
		}
	}
}

// TestComposePostgresNeedsBothSecretFiles 는 시크릿 자리 둘이 기본값 없이 필수인지 잰다. 기본값을 두면 그 자리는 늘 홈 안(서버가
// FD_REPOS 로 읽는 자리)이 된다 - 비면 `up` 이 그 변수 이름을 대고 서야 한다.
func TestComposePostgresNeedsBothSecretFiles(t *testing.T) {
	for _, drop := range []string{"FD_PG_SERVER_PGPASS_HOST", "FD_PG_BACKUP_PGPASS_HOST"} {
		env := pgLayerEnv()
		delete(env, drop)
		_, stderr, err := renderCompose(t, env, nil, "compose.yaml", "compose.postgres.yaml")
		if err == nil {
			t.Fatalf("%s 없이도 Postgres 겹이 렌더됐다 - 시크릿 자리는 필수여야 한다", drop)
		}
		if !strings.Contains(stderr, drop) {
			t.Fatalf("%s 가 빠진 렌더의 오류가 그 이름을 안 댄다:\n%s", drop, stderr)
		}
	}
}

// TestPostgresClientMatchesTheLedgerServer 는 서버 이미지(server/Dockerfile 의 마지막 단)가 원장 DB 와 같거나 높은 큰 판의
// psql · pg_dump · pg_restore 를 갖는지 잰다. 낮으면 `fd migrate` 의 판올림 전 덤프가 서버 판을 거절해 판을 못 올린다.
func TestPostgresClientMatchesTheLedgerServer(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pluginRoot(t), "server", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	stages := regexp.MustCompile(`(?m)^FROM\s`).Split(string(raw), -1)
	runtime := stages[len(stages)-1]
	pkg := regexp.MustCompile(`(?m)^RUN apk add --no-cache .*\bpostgresql(\d+)-client\b`).FindStringSubmatch(runtime)
	if pkg == nil {
		t.Fatal("서버 이미지의 마지막 단에 `apk add … postgresqlNN-client` 가 없다 - 판올림 전 덤프 · 되돌리기 · 되살리기가 그 도구를 쓴다")
	}
	client, _ := strconv.Atoi(pkg[1])
	image := renderPG(t).Services["db"].Image
	server, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(image, "postgres:"), ".", 2)[0])
	if err != nil {
		t.Fatalf("db 이미지 %q 에서 큰 판을 못 읽었다", image)
	}
	if client < server {
		t.Fatalf("서버 이미지의 Postgres 도구가 %d 판이고 원장 DB 가 %d 판이다 - pg_dump 는 자기보다 높은 서버를 거절한다", client, server)
	}
}

// ── deploy/db-roles.sh ─────────────────────────────────────────────────────────

func dbRolesScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(pluginRoot(t), "deploy", "db-roles.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("db-roles 스크립트가 없다: %v", err)
	}
	return p
}

// runDBRoles 는 스크립트를 env 만 준 채(이 프로세스의 환경을 안 물려준다) 돌리고 출력 둘과 종료 코드를 낸다.
func runDBRoles(t *testing.T, env []string, args ...string) (stdout, stderr string, rc int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{dbRolesScript(t)}, args...)...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("db-roles 를 못 돌렸다: %v", err)
		}
		rc = ee.ExitCode()
	}
	return out.String(), errb.String(), rc
}

func writePgpass(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "pgpass")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDBRolesPgpassReadsOnlyTheCommonForm 은 password 동작이 libpq 와 pgx 가 똑같이 읽는 꼴만 받고, 두 라이브러리처럼 맞는 첫
// 줄을 고르는지 잰다. 어긋난 줄은 고른 줄이 아니어도 거절한다 - 읽는 쪽마다 다른 줄을 고를 수 있는 파일이기 때문이다.
//
// 어긋난 줄은 파일 · 줄 번호로 대고 그 줄의 값(비밀번호일 수 있다)은 안 낸다 - 어긋난 줄의 값에 Secret 을 심어 잰다.
func TestDBRolesPgpassReadsOnlyTheCommonForm(t *testing.T) {
	env := []string{"PATH=" + os.Getenv("PATH"), "PGDATABASE=flightdeck"}
	for _, c := range []struct {
		name, body, user, want string
		rc, badLine            int
	}{
		{"맞는 첫 줄", "# 주석\n\ndb:5432:flightdeck:fd_server:Pa$$w0rd!*\ndb:5432:flightdeck:fd_server:second\n", "fd_server", "Pa$$w0rd!*", 0, 0},
		{"별표는 아무 값", "*:*:*:fd_backup:bk\n", "fd_backup", "bk", 0, 0},
		{"다른 DB 줄은 안 고른다", "db:5432:other:fd_server:x\n", "fd_server", "", 3, 0},
		{"칸 여섯(비밀번호의 콜론)", "db:5432:flightdeck:fd_server:Secret:b\n", "fd_server", "", 1, 1},
		{"공백", "db:5432:flightdeck:fd_server:Secret b\n", "fd_server", "", 1, 1},
		{"앞 공백 주석", " # Secret\ndb:5432:flightdeck:fd_server:x\n", "fd_server", "", 1, 1},
		{"CR", "db:5432:flightdeck:fd_server:Secret\r\n", "fd_server", "", 1, 1},
		{"역빗금", `db:5432:flightdeck:fd_server:Secret\:b` + "\n", "fd_server", "", 1, 1},
		{"빈 비밀번호", "db:5432:flightdeck:fd_server:\n", "fd_server", "", 1, 1},
		{"ASCII 밖", "db:5432:flightdeck:fd_server:Secret비밀\n", "fd_server", "", 1, 1},
		{"고르지 않은 줄의 어긋남", "db:5432:flightdeck:fd_server:ok\nSecret line\n", "fd_server", "", 1, 2},
	} {
		p := writePgpass(t, c.body, 0o600)
		out, stderr, rc := runDBRoles(t, env, "password", p, c.user)
		if rc != c.rc || strings.TrimSuffix(out, "\n") != c.want {
			t.Errorf("%s: rc=%d 출력=%q, 기대 rc=%d 출력=%q\n%s", c.name, rc, out, c.rc, c.want, stderr)
		}
		if c.badLine > 0 && !strings.Contains(stderr, p+":"+strconv.Itoa(c.badLine)+":") {
			t.Errorf("%s: 오류가 %s:%d 를 안 댄다: %q", c.name, p, c.badLine, stderr)
		}
		if strings.Contains(stderr, "Secret") {
			t.Errorf("%s: 오류에 어긋난 줄의 값이 실렸다: %q", c.name, stderr)
		}
	}
}

// fakePsql 은 받은 인자를 한 줄에 하나씩, 표준 입력을 그대로 적고 FAKE_PSQL_RC 로 끝난다.
const fakePsql = `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a"; done > "$FAKE_PSQL_DIR/argv"
cat > "$FAKE_PSQL_DIR/stdin"
env > "$FAKE_PSQL_DIR/env"
exit "${FAKE_PSQL_RC:-0}"
`

type dbRolesRig struct {
	t                *testing.T
	dir              string
	srvFile, bakFile string
	srvPass, bakPass string
	base             map[string]string
}

func newDBRolesRig(t *testing.T) *dbRolesRig {
	t.Helper()
	r := &dbRolesRig{t: t, dir: t.TempDir(), srvPass: "Srv-0123456789abcdef", bakPass: "Bak-fedcba9876543210"}
	bin := filepath.Join(r.dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "psql"), []byte(fakePsql), 0o755); err != nil {
		t.Fatal(err)
	}
	r.srvFile = writePgpass(t, "db:5432:flightdeck:fd_server:"+r.srvPass+"\n", 0o600)
	r.bakFile = writePgpass(t, "db:5432:flightdeck:fd_backup:"+r.bakPass+"\n", 0o600)
	r.base = map[string]string{
		"PATH": bin + ":" + os.Getenv("PATH"), "FAKE_PSQL_DIR": r.dir,
		"PGHOST": "/var/run/postgresql", "PGPORT": "5432", "PGUSER": "postgres", "PGDATABASE": "flightdeck",
		"FD_PG_SERVER_USER": "fd_server", "FD_PG_BACKUP_USER": "fd_backup",
		"FD_PG_SERVER_PGPASS": r.srvFile, "FD_PG_BACKUP_PGPASS": r.bakFile,
	}
	return r
}

func (r *dbRolesRig) run(over map[string]string) (string, string, int) {
	r.t.Helper()
	_ = os.Remove(filepath.Join(r.dir, "argv"))
	var env []string
	for k, v := range r.base {
		if o, ok := over[k]; ok {
			v = o
		}
		env = append(env, k+"="+v)
	}
	return runDBRoles(r.t, env)
}

func (r *dbRolesRig) read(name string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		r.t.Fatalf("가짜 psql 의 %s 가 없다(psql 이 안 불렸다): %v", name, err)
	}
	return string(b)
}

// TestDBRolesKeepsPasswordsOutOfArgvAndEnv 는 계정 맞추기가 psql 을 부를 때 비밀번호를 인자 · 환경 변수 · SQL 본문 어디에도
// 싣지 않고(psql 이 역따옴표로 이 스크립트의 password 동작을 불러 받는다), 한 트랜잭션 안에서 만들기 · 멤버십 · 재기 · 비밀번호를
// 하는지 잰다. 실제 서버에서의 동작(거절 갈래 포함)은 dbroles_pg_test.go 가 이 스크립트를 시험 PG 클러스터에 그대로 돌려 잰다
// - 여기는 psql 에 넘기는 모양이다.
func TestDBRolesKeepsPasswordsOutOfArgvAndEnv(t *testing.T) {
	r := newDBRolesRig(t)
	out, stderr, rc := r.run(nil)
	if rc != 0 || !strings.Contains(out, "맞췄다") {
		t.Fatalf("rc=%d out=%q\n%s", rc, out, stderr)
	}
	argv, env, sql := r.read("argv"), r.read("env"), r.read("stdin")
	for _, pw := range []string{r.srvPass, r.bakPass} {
		for what, s := range map[string]string{"인자": argv, "환경 변수": env, "SQL 본문": sql, "출력": out + stderr} {
			if strings.Contains(s, pw) {
				t.Errorf("비밀번호가 psql 의 %s 에 실렸다", what)
			}
		}
	}
	mustContain(t, "psql 인자", argv, "-X\n", "ON_ERROR_STOP=1\n", "--no-password\n", "srv=fd_server\n", "bak=fd_backup\n",
		"srv_file="+r.srvFile+"\n", "bak_file="+r.bakFile+"\n", "self="+dbRolesScript(t)+"\n")
	mustContain(t, "SQL 본문", sql,
		"\\set srv_pw `sh :'self' password :'srv_file' :'srv'`",
		"\\set bak_pw `sh :'self' password :'bak_file' :'bak'`",
		"BEGIN;", "GRANT fd_app TO :\"srv\";", "GRANT pg_read_all_data TO :\"bak\";",
		"RAISE EXCEPTION 'pgpass 파일에서 비밀번호를 못 읽었다'",
		"ALTER ROLE :\"srv\" PASSWORD :'srv_pw';", "ALTER ROLE :\"bak\" PASSWORD :'bak_pw';", "COMMIT;")
	if strings.Index(sql, "RAISE EXCEPTION '접속 계정이") > strings.Index(sql, "ALTER ROLE :\"srv\" PASSWORD") {
		t.Error("계정을 재기 전에 비밀번호를 둔다 - 어긋난 계정에 비밀번호를 주지 않는다")
	}
	// psql 이 실패하면 스크립트도 실패한다.
	r.base["FAKE_PSQL_RC"] = "3"
	if _, _, rc := r.run(nil); rc != 1 {
		t.Errorf("psql 이 3 으로 끝났는데 스크립트가 rc=%d 다 - 1 이어야 한다", rc)
	}
}

// TestDBRolesRefusesBadSettings 는 psql 을 부르기 전에 설정을 거절하는 갈래(rc 2)를 잰다. 거절하면 psql 이 안 불린다.
func TestDBRolesRefusesBadSettings(t *testing.T) {
	r := newDBRolesRig(t)
	loose := writePgpass(t, "db:5432:flightdeck:fd_backup:x\n", 0o640)
	other := writePgpass(t, "db:5432:flightdeck:someone:x\n", 0o600)
	for _, c := range []struct {
		name string
		over map[string]string
		want string
	}{
		{"같은 계정", map[string]string{"FD_PG_BACKUP_USER": "fd_server"}, "서버 계정과 덤프 계정이 같다"},
		{"초사용자 이름", map[string]string{"FD_PG_SERVER_USER": "postgres"}, "계정 이름으로 못 쓴다"},
		{"묶음 역할 이름", map[string]string{"FD_PG_BACKUP_USER": "fd_app"}, "계정 이름으로 못 쓴다"},
		{"예약 접두", map[string]string{"FD_PG_BACKUP_USER": "pg_x"}, "계정 이름으로 못 쓴다"},
		{"따옴표가 필요한 이름", map[string]string{"FD_PG_SERVER_USER": "Fd-Server"}, "소문자 · 숫자 · _ 만"},
		{"DB 이름", map[string]string{"PGDATABASE": "a:b"}, "소문자 · 숫자 · _ 만"},
		{"남에게 열린 pgpass", map[string]string{"FD_PG_BACKUP_PGPASS": loose}, "0600 이어야 한다"},
		{"맞는 줄이 없다", map[string]string{"FD_PG_SERVER_PGPASS": other}, "맞는 줄이 없다"},
		{"없는 파일", map[string]string{"FD_PG_SERVER_PGPASS": filepath.Join(r.dir, "none")}, "못 읽는다"},
	} {
		out, stderr, rc := r.run(c.over)
		if rc != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: rc=%d stderr=%q out=%q - rc 2 와 %q 가 기대다", c.name, rc, stderr, out, c.want)
		}
		if _, err := os.Stat(filepath.Join(r.dir, "argv")); err == nil {
			t.Errorf("%s: 설정을 거절하고도 psql 을 불렀다", c.name)
		}
	}
}
