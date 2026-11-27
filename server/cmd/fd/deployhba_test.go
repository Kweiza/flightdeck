package main

// deploy/pg_hba.conf(원장 DB 의 접속 규칙)를 실제 Postgres 로 재는 시험이다.
//
// ★ 렌더한 db 서비스(build · command · initdb 인자 · healthcheck)를 그대로 일회용 컨테이너로 띄운다 - 파일의 글자가 아니라
//   compose 가 실제로 띄우는 모양이 거절하는지 잰다. command 에서 hba_file 이 빠지면 볼륨 안 pg_hba.conf 로 떠서 여기서 빨갛다.
// ★ 이미지는 렌더한 build 의 자리를 **umask 077 체크아웃처럼**(파일 0600 · 디렉토리 0700) 베낀 곳에서 짓고, 띄우기 전에 그 자리를
//   지운다. 굽는 줄이 모드를 정하지 않으면(--chmod) postgres 사용자가 규칙을 못 읽어 db 가 안 뜨고, 컨테이너가 판 디렉토리에
//   기대면(bind) 지운 자리 때문에 안 뜬다 - 둘 다 여기서 빨갛다.
// ★ 두 볼륨을 잰다: 이 compose 의 진입점이 처음 만드는 볼륨, 그리고 다른 길(initdb --auth=trust)로 이미 만든 볼륨. 둘째는 볼륨
//   안 규칙이 127.0.0.1 · ::1 을 비밀번호 없이 연다 - 그래도 초사용자의 TCP 는 거절되고 앱 계정은 비밀번호를 내야 한다.
// ★ 첫째는 초사용자 비밀번호를 **아는** 값으로 만든다(compose 는 아무도 모르는 난수다). 「비밀번호를 몰라서 못 붙는다」가 아니라
//   접속 규칙이 거절하는 것을 잰다.
// ★ 컨테이너 · 볼륨 이름은 시험마다 새로 짓고(fdtest-hba-…) 끝나면 지운다. 포트를 내지 않고 운영의 이름 · 볼륨 · 소켓을 안 쓴다.
//   docker 데몬이 없으면 **실패**한다.

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPGHBARejectsSuperuserOverTCP(t *testing.T) {
	db := renderPG(t).Services["db"]
	var dataDir string
	for _, v := range db.Volumes {
		switch {
		case v.Source == "pgdata":
			dataDir = v.Target
		case v.Type == "bind":
			t.Fatalf("db 가 %s 를 %s 에 bind 한다 - 이미지에 굽는다(TestComposeServicesDoNotBindThePluginDirectory)", v.Source, v.Target)
		}
	}
	initdbArgs, _ := db.env("POSTGRES_INITDB_ARGS")
	pgDB, _ := db.env("POSTGRES_DB")
	hc := db.Healthcheck.Test
	if dataDir == "" || db.Build == nil || len(hc) < 2 || hc[0] != "CMD" {
		t.Fatalf("렌더한 db 에서 데이터 자리(%q) · build(%+v) · healthcheck(%q)를 못 읽었다", dataDir, db.Build, hc)
	}
	hcUser := hc[slices.Index(hc, "-U")+1]
	image := buildPGLayerUnderUmask077(t, db.Build.Context, db.Build.Dockerfile)
	// 구운 파일은 umask 077 의 원천에서도 누구나 읽는다(0644) - db 의 postgres 사용자, FD_UID 로 도는 db-roles · db-backup.
	for _, p := range []string{pgHBATarget, "/usr/local/bin/fd-db-roles", "/usr/local/bin/fd-db-backup"} {
		out, err := exec.Command("docker", "run", "--rm", "--entrypoint", "stat", image, "-c", "%a", p).CombinedOutput()
		if err != nil || strings.TrimSpace(string(out)) != "644" {
			t.Errorf("이미지의 %s 모드가 %q 다(err=%v) - 0644 여야 umask 077 체크아웃에서도 읽힌다", p, strings.TrimSpace(string(out)), err)
		}
	}

	for _, tc := range []struct {
		name     string
		existing bool // 다른 길(initdb --auth=trust)로 이미 만든 볼륨에 띄운다
	}{{"새 볼륨", false}, {"trust 로 이미 만든 볼륨", true}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			name := "fdtest-hba-" + randHex(t, 6)
			superPass := "S-" + randHex(t, 8)
			run := []string{"run", "-d", "--name", name, "--sysctl", "net.ipv6.conf.lo.disable_ipv6=0",
				"-e", "POSTGRES_DB=" + pgDB, "-e", "POSTGRES_INITDB_ARGS=" + initdbArgs, "-e", "POSTGRES_PASSWORD=" + superPass}
			if tc.existing {
				vol := name + "-data"
				mustDocker(t, "volume", "create", vol)
				t.Cleanup(func() { mustDocker(t, "volume", "rm", vol) }) // 컨테이너보다 늦게 돈다(정리는 거꾸로 돈다)
				mustDocker(t, "run", "--rm", "-u", "postgres", "-v", vol+":"+dataDir, image,
					"initdb", "-D", dataDir, "-U", "postgres", "--auth=trust")
				run = append(run, "-v", vol+":"+dataDir)
			}
			t.Cleanup(func() { mustDocker(t, "rm", "-f", "-v", name) })
			run = append(run, image)
			mustDocker(t, append(run, db.Command...)...)
			waitTCP(t, name, hc[1:])

			// 초사용자: TCP 는 IPv4 · IPv6 · 복제 모두 접속 규칙의 reject 줄이 거절한다. 비밀번호를 알아도.
			for _, host := range []string{"127.0.0.1", "::1"} {
				for _, conn := range []string{"dbname=postgres", "dbname=postgres replication=database"} {
					dsn := "host=" + host + " user=postgres " + conn
					out, rc := dockerExec(name, []string{"PGPASSWORD=" + superPass}, "psql", "-X", "-Atc", "select 1", dsn)
					if rc == 0 || !strings.Contains(out, "pg_hba.conf rejects") {
						t.Errorf("초사용자가 TCP(%s)로 붙었거나 접속 규칙이 아닌 까닭으로 막혔다(rc=%d) - reject 줄이 거절해야 한다:\n%s", dsn, rc, out)
					}
				}
			}
			// 초사용자: 소켓은 trust 다(관리 컨테이너 migrate · db-roles 의 길).
			if out, rc := dockerExec(name, nil, "psql", "-X", "-U", "postgres", "-d", "postgres", "-Atc", "select 1"); rc != 0 || out != "1\n" {
				t.Fatalf("초사용자가 소켓으로 못 붙었다(rc=%d):\n%s", rc, out)
			}

			// healthcheck: 렌더한 그대로 0 으로 끝나고(계정이 아직 없을 때 · 있을 때), 로그에 그 계정의 줄을 안 남긴다.
			appPass := "A-" + randHex(t, 8)
			for _, step := range []string{"계정 없음", "계정 있음"} {
				if step == "계정 있음" {
					if out, rc := dockerExec(name, nil, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "postgres",
						"-c", "CREATE ROLE "+hcUser+" LOGIN PASSWORD '"+appPass+"'"); rc != 0 {
						t.Fatalf("앱 계정을 못 만들었다(rc=%d):\n%s", rc, out)
					}
				}
				if out, rc := dockerExec(name, nil, hc[1:]...); rc != 0 {
					t.Errorf("렌더한 healthcheck(%q)가 %s 에서 %d 로 끝났다:\n%s", hc, step, rc, out)
				}
			}
			if logs, _ := exec.Command("docker", "logs", name).CombinedOutput(); bytes.Contains(logs, []byte(`"`+hcUser+`"`)) {
				t.Errorf("healthcheck 가 로그에 %s 의 줄을 남겼다 - 10초마다 쌓인다:\n%s", hcUser, logs)
			}

			// 앱 계정: TCP 는 IPv4 · IPv6 모두 SCRAM 이다 - 맞는 비밀번호는 붙고, 틀린 비밀번호는 못 붙는다(볼륨 안 trust 줄을 안 탄다).
			for _, host := range []string{"127.0.0.1", "::1"} {
				args := []string{"psql", "-X", "-h", host, "-U", hcUser, "-d", "postgres", "-Atc", "select 1"}
				if out, rc := dockerExec(name, []string{"PGPASSWORD=" + appPass}, args...); rc != 0 || out != "1\n" {
					t.Errorf("앱 계정이 맞는 비밀번호로 TCP(%s)에 못 붙었다(rc=%d):\n%s", host, rc, out)
				}
				if out, rc := dockerExec(name, []string{"PGPASSWORD=wrong-" + appPass}, args...); rc == 0 || !strings.Contains(out, "password authentication failed") {
					t.Errorf("앱 계정이 틀린 비밀번호로 TCP(%s)에 붙었거나 다른 까닭으로 막혔다(rc=%d) - SCRAM 이어야 한다:\n%s", host, rc, out)
				}
			}
		})
	}
}

// buildPGLayerUnderUmask077 는 build 자리 ctx 를 umask 077 체크아웃의 모드(파일 0600 · 디렉토리 0700)로 임시 자리에 베껴 시험
// 이름(fdtest-hba-…)의 이미지로 짓고, 그 임시 자리를 지운 뒤 이미지 이름을 낸다. 이미지는 시험이 끝나면 지운다. 운영 이미지
// 이름(flightdeck-pg:local)은 짓지도 건드리지도 않는다.
func buildPGLayerUnderUmask077(t *testing.T, ctx, dockerfile string) string {
	t.Helper()
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	tmp, err := os.MkdirTemp("", "fdtest-hba-ctx-")
	if err != nil {
		t.Fatal(err)
	}
	copied := 0
	err = filepath.WalkDir(ctx, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(ctx, p)
		dst := filepath.Join(tmp, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		copied++
		return os.WriteFile(dst, raw, 0o600)
	})
	if err != nil || copied == 0 {
		os.RemoveAll(tmp)
		t.Fatalf("build 자리 %s 를 베끼지 못했다(%d 파일): %v", ctx, copied, err)
	}
	if err := os.Chmod(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	image := "fdtest-hba-" + randHex(t, 6) + ":local"
	build := exec.Command("docker", "build", "-q", "-t", image, "-f", filepath.Join(tmp, dockerfile), tmp)
	build.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	out, err := build.CombinedOutput()
	if rmErr := os.RemoveAll(tmp); rmErr != nil {
		t.Fatal(rmErr)
	}
	if err != nil {
		t.Fatalf("Postgres 겹 이미지를 못 지었다: %v\n%s", err, out)
	}
	t.Cleanup(func() { mustDocker(t, "rmi", "-f", image) }) // 하위 시험(병렬)이 다 끝난 뒤에 돈다
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("임시 build 자리 %s 가 남았다 - 지운 자리로 뜨는지 재지 못한다", tmp)
	}
	return image
}

// mustDocker 는 docker 명령 하나를 돌리고, 실패하면 시험을 멈춘다(데몬이 없을 때도 - 건너뛰지 않는다).
func mustDocker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %s: %v\n%s", args[0], err, out)
	}
}

// dockerExec 는 컨테이너 안에서 명령 하나를 돌려 합친 출력과 종료 코드를 낸다(-1: docker 자체가 못 돌았다).
func dockerExec(name string, env []string, args ...string) (string, int) {
	cmdArgs := []string{"exec"}
	for _, kv := range env {
		cmdArgs = append(cmdArgs, "-e", kv)
	}
	cmdArgs = append(append(cmdArgs, name), args...)
	out, err := exec.Command("docker", cmdArgs...).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		return string(out) + err.Error(), -1
	}
	return string(out), 0
}

// waitTCP 는 진짜 서버가 TCP 를 들을 때까지 기다린다. 첫 초기화의 임시 서버는 소켓만 듣는다 - healthcheck 가 TCP 로 재는 까닭과 같다.
func waitTCP(t *testing.T, name string, probe []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		out, rc := dockerExec(name, nil, probe...)
		if rc == 0 {
			return
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
			t.Fatalf("db 가 2분 안에 TCP 를 안 들었다(%s):\n%s", out, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
