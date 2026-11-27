package main

// README 의 Postgres 절차(「업그레이드: SQLite 원장을 Postgres 로」 · 「켜기 전에 섰을 때」 · 「72시간 안에 되돌리기」)를 README 의
// 글자 그대로 떼어, 대역 docker 로 돌리는 시험이다.
//
// ★ 글자가 아니라 **도는 모양**을 잰다. 이 절차들은 한때 단계마다 `&&` 사슬이었다 - 사슬은 그 단계 안에서만 서고, 다음 단계는
//   앞 단계가 섰어도 돌았다(옛 서버를 못 내렸는데 옮기고, 계정 검사가 섰는데 선택 파일을 켠다). 그래서 셸이 실제로 어디서 서는지,
//   선 뒤에 무엇이 남는지(선택 파일 · 이관 표지 · 원장 파일)를 실제 셸(bash, 있으면 zsh)로 잰다.
// ★ 셸 함수(fdc · fdrepos · fdwhere · srvsql · nodump)도 README 의 정의를 그대로 쓴다 - 대역은 docker 하나다. 대역은 부른 것을
//   (자리 · 넘긴 선택 파일 · 인자) 적고, 상태 디렉토리로 도는 서버를 흉내 내고, 이관 · 역이관이 남기는 파일을 만든다. 운영
//   docker · 운영 자리(/etc/flightdeck · 홈)는 건드리지 않는다: 선택 파일 자리를 시험 자리로 바꿔 쓰고 HOME 을 시험 자리로 준다.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readmeProcs 는 README 하나에서 뗀 절차 덩이다.
type readmeProcs struct {
	defs, upgrade, measure, back, rollback string
}

// readmeBlocks 는 글의 ```bash 블록들이다.
func readmeBlocks(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile("(?s)```bash\n(.*?)\n```").FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func loadReadmeProcs(t *testing.T, readme string) readmeProcs {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pluginRoot(t), readme))
	if err != nil {
		t.Fatal(err)
	}
	blocks := readmeBlocks(string(raw))
	one := func(what, marker string) string {
		t.Helper()
		var hit []string
		for _, b := range blocks {
			if strings.Contains(b, marker) {
				hit = append(hit, b)
			}
		}
		if len(hit) != 1 {
			t.Fatalf("%s 에서 %s 덩이(%q 를 담은 bash 블록)가 %d 개다 - 하나여야 한다", readme, what, marker, len(hit))
		}
		return hit[0]
	}
	var p readmeProcs
	// 셸 함수: 준비 5 의 fdc · fdrepos · fdwhere(블록 끝까지), 관리 명령의 sql · srvsql · nodump 줄.
	setup := one("준비", "fdc() {")
	p.defs = setup[strings.Index(setup, "fdc() {"):] + "\n"
	for _, ln := range strings.Split(one("관리 명령", "nodump() {"), "\n") {
		if strings.HasPrefix(ln, "sql() {") || strings.HasPrefix(ln, "srvsql() {") || strings.HasPrefix(ln, "nodump() {") {
			p.defs += ln + "\n"
		}
	}
	p.upgrade = one("업그레이드 2-8", "import-sqlite --from /data/fd.db --dry-run")
	p.measure = one("켜기 전에 섰을 때 - 재기", "migrate verify --from /data/fd.db; then")
	p.back = one("켜기 전에 섰을 때 - 돌아가기", `rm -f "$HOME/.flightdeck/fd.db.moved-to-postgres"`)
	p.rollback = one("72시간 되돌리기 2-6", "export-sqlite --to /data/fd-back.db --dry-run")
	// 두 절차의 1 은 fdwhere 한 줄이다 - 시험의 「처음 돌기」가 그 줄을 그대로 앞에 붙인다.
	n := 0
	for _, b := range blocks {
		code := ""
		for _, ln := range strings.Split(b, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(ln), "#") {
				code += ln
			}
		}
		if strings.TrimSpace(code) == "fdwhere" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%s 에서 1 단계(fdwhere 한 줄) 블록이 %d 개다 - 업그레이드 · 72시간 되돌리기에 하나씩이다", readme, n)
	}
	return p
}

// fakeDocker 는 대역 docker 다. 부른 것을 FDT_LOG 에 한 줄씩 적는다: `PWD=<자리> ENV=<COMPOSE_ENV_FILES> ARGS=<인자>`.
// FDT_FAIL(정규식)에 맞는 인자면 1 로 끝난다. FDT_STATE/up 이 있으면 서버 컨테이너(flightdeck)가 돈다.
const fakeDocker = `#!/bin/sh
printf 'PWD=%s ENV=%s ARGS=%s\n' "$PWD" "${COMPOSE_ENV_FILES-}" "$*" >> "$FDT_LOG"
if [ -n "${FDT_FAIL-}" ] && printf '%s' "$*" | grep -Eq -- "$FDT_FAIL"; then echo "대역 docker: 실패를 낸다($*)" >&2; exit 1; fi
st=$FDT_STATE
case "$1" in
compose)
  case " $* " in
  *" import-sqlite "*" --dry-run "*|*" export-sqlite "*" --dry-run "*) ;;
  *" import-sqlite "*) : > "$HOME/.flightdeck/fd.db.moved-to-postgres" ;;
  *" export-sqlite "*) [ ! -e "$HOME/.flightdeck/fd-back.db" ] || exit 1; echo back > "$HOME/.flightdeck/fd-back.db" ;;
  *" ps -aq db-roles "*) echo rolesid ;;
  *" down "*) rm -f "$st/up" ;;
  esac
  case "$*" in *" up -d"|*" up -d --build") : > "$st/up" ;; esac
  exit 0 ;;
inspect)
  [ -e "$st/up" ] || { echo "Error: No such object: flightdeck" >&2; exit 1; }
  case "$*" in
  *working_dir*) echo "$FDT_W" ;;
  *compose.project*) echo "$FDT_P" ;;
  *Mounts*) printf '%s\n' /repos/one /repos/two ;;
  esac
  exit 0 ;;
ps)
  case "$*" in
  *service=flightdeck*) [ ! -e "$st/up" ] || echo srvid ;;
  *service=db-backup*) [ -z "${FDT_DUMPS-}" ] || echo "$FDT_DUMPS" ;;
  esac
  exit 0 ;;
wait) echo "${FDT_WAIT:-0}"; exit 0 ;;
esac
exit 0
`

// procRig 는 시험 한 판의 자리다: 홈(~/.flightdeck 가 /data 다) · 선택 파일 자리 · 대역 docker · 상태 · 기록.
type procRig struct {
	t                          *testing.T
	shell                      string
	procs                      readmeProcs
	home, etc, state, log, bin string
	work, old                  string // 이 판의 자리(절차를 돌리는 곳) · 도는 서버의 라벨이 가리키는 자리
	env                        map[string]string
}

func newProcRig(t *testing.T, shell string, procs readmeProcs) *procRig {
	t.Helper()
	root := t.TempDir()
	r := &procRig{t: t, shell: shell, procs: procs, env: map[string]string{}}
	for _, d := range []*string{&r.home, &r.etc, &r.state, &r.bin, &r.work, &r.old} {
		*d = filepath.Join(root, "d"+randHex(t, 4))
		if err := os.MkdirAll(*d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r.log = filepath.Join(root, "docker.log")
	if err := os.MkdirAll(filepath.Join(r.home, ".flightdeck"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.bin, "docker"), []byte(fakeDocker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.bin, "curl"), []byte("#!/bin/sh\necho '{\"db_ok\":true}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *procRig) data(name string) string { return filepath.Join(r.home, ".flightdeck", name) }
func (r *procRig) on() string              { return filepath.Join(r.etc, "compose.env") }
func (r *procRig) off() string             { return r.on() + ".off" }

func (r *procRig) write(path, body string) {
	r.t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *procRig) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (r *procRig) read(path string) string {
	r.t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		r.t.Fatalf("%s 를 못 읽었다: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

// serverUp 은 도는 서버(라벨 프로젝트 p · 자리 w)를 둔다.
func (r *procRig) serverUp(p, w string) {
	r.write(filepath.Join(r.state, "up"), "")
	r.env["FDT_P"], r.env["FDT_W"] = p, w
}

// run 은 셸 함수 정의 뒤 cwd 에서 앞줄(prelude)과 덩이(block)를 돌린다. 덩이의 부분 셸이 닫힌 바로 뒤에 그 종료 코드를 찍어
// 낸다(덩이 뒤의 curl 같은 줄이 코드를 덮지 않게). 이 판의 docker 기록 줄과 출력도 낸다.
func (r *procRig) run(cwd, prelude, block string) (rc string, calls []string, out string) {
	r.t.Helper()
	sub := func(s string) string { return strings.ReplaceAll(s, "/etc/flightdeck", r.etc) }
	body := sub(block) + "\n"
	if !strings.Contains(body, "\n)\n") {
		r.t.Fatalf("덩이가 `)` 줄로 닫히는 부분 셸이 아니다:\n%s", block)
	}
	body = strings.Replace(body, "\n)\n", "\n)\necho \"BLOCK_RC=$?\"\n", 1)
	script := sub(r.procs.defs) + "\ncd '" + cwd + "' || exit 99\n" + prelude + "\n" + body
	_ = os.Remove(r.log)
	cmd := exec.Command(r.shell, "-c", script)
	cmd.Dir = cwd
	env := []string{"HOME=" + r.home, "PATH=" + r.bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C.UTF-8",
		"FDT_LOG=" + r.log, "FDT_STATE=" + r.state}
	for k, v := range r.env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	b, _ := cmd.CombinedOutput()
	out = string(b)
	if m := regexp.MustCompile(`BLOCK_RC=(\d+)`).FindStringSubmatch(out); m != nil {
		rc = m[1]
	} else {
		r.t.Fatalf("덩이의 종료 코드가 안 나왔다(셸이 덩이 전에 섰다):\n%s", out)
	}
	if raw, err := os.ReadFile(r.log); err == nil {
		calls = strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	return rc, calls, out
}

// composeCalls 는 기록 가운데 compose 호출만 낸다.
func composeCalls(calls []string) []string {
	var out []string
	for _, c := range calls {
		if strings.Contains(c, " ARGS=compose ") {
			out = append(out, c)
		}
	}
	return out
}

// called 는 compose 호출 가운데 인자가 args 로 끝나는 첫 줄이다(없으면 "").
func called(calls []string, args string) string {
	for _, c := range composeCalls(calls) {
		if strings.HasSuffix(c, " "+args) {
			return c
		}
	}
	return ""
}

// procShells 는 절차를 돌릴 셸이다 - bash 는 꼭, zsh 는 있으면(README 의 함수를 셸 설정에 두는 사람의 셸이다).
func procShells(t *testing.T) []string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash 가 없다 - README 절차를 셸로 재지 못한다: %v", err)
	}
	out := []string{bash}
	if zsh, err := exec.LookPath("zsh"); err == nil {
		out = append(out, zsh)
	}
	return out
}

// TestREADMEProceduresStopWhole 은 README(정본 · 번역본)의 세 절차가 첫 실패에서 통째로 서고, 선 자리에 맞는 것만 남기며, 다시
// 붙여 넣으면 된 단계를 건너뛰는지 잰다.
func TestREADMEProceduresStopWhole(t *testing.T) {
	for _, readme := range []string{"README.ko.md", "README.md"} {
		procs := loadReadmeProcs(t, readme)
		for _, shell := range procShells(t) {
			t.Run(readme+"/"+filepath.Base(shell), func(t *testing.T) {
				t.Run("업그레이드", func(t *testing.T) { checkUpgrade(t, shell, procs) })
				t.Run("켜기 전에 섰을 때", func(t *testing.T) { checkBeforeSwitchOn(t, shell, procs) })
				t.Run("72시간 되돌리기", func(t *testing.T) { checkRollback(t, shell, procs) })
			})
		}
	}
}

// upgradeRig 는 업그레이드 전의 자리다: 꺼 둔 선택 파일, SQLite 원장, 옛 판 자리에서 도는 서버.
func upgradeRig(t *testing.T, shell string, procs readmeProcs) *procRig {
	r := newProcRig(t, shell, procs)
	r.write(r.off(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
	r.write(r.data("fd.db"), "old")
	r.serverUp("oldp", r.old)
	return r
}

func checkUpgrade(t *testing.T, shell string, procs readmeProcs) {
	t.Run("끝까지 돈다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
		if rc != "0" {
			t.Fatalf("덩이가 %s 로 섰다:\n%s", rc, out)
		}
		if !r.exists(r.on()) || r.exists(r.off()) || !r.exists(r.data("fd.db.moved-to-postgres")) {
			t.Fatalf("끝난 뒤 선택 파일 · 표지가 맞지 않다(켬 %v · 끔 %v · 표지 %v)", r.exists(r.on()), r.exists(r.off()), r.exists(r.data("fd.db.moved-to-postgres")))
		}
		for _, want := range []struct{ args, pwd, env string }{
			{"-p oldp down", r.old, ""},
			{"run --rm --no-deps migrate migrate --db /data/fd.db", r.work, ""},
			{"run --rm migrate migrate --to 18", r.work, r.off()},
			{"run --rm --no-deps migrate migrate import-sqlite --from /data/fd.db", r.work, r.off()},
			{"run --rm --no-deps migrate migrate verify --from /data/fd.db", r.work, r.off()},
			{"up -d", r.work, r.on()},
		} {
			c := called(calls, want.args)
			if c == "" || !strings.HasPrefix(c, "PWD="+want.pwd+" ENV="+want.env+" ARGS=") {
				t.Errorf("`%s` 가 자리 %s · 선택 파일 %q 로 안 돌았다: %q", want.args, want.pwd, want.env, c)
			}
		}
		if n := len(composeCalls(calls)); !strings.HasSuffix(composeCalls(calls)[n-1], " ARGS=compose ps -a") {
			t.Errorf("마지막 compose 호출이 ps -a 가 아니다: %q", composeCalls(calls)[n-1])
		}
	})
	t.Run("옛 서버를 못 내리면 아무것도 안 옮긴다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		r.env["FDT_FAIL"] = "-p oldp down$"
		rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
		if rc == "0" || called(calls, "build") != "" || strings.Contains(strings.Join(calls, "\n"), "import-sqlite") {
			t.Fatalf("내리기가 섰는데 덩이가 이어 갔다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		if !r.exists(r.off()) || r.exists(r.on()) || r.exists(r.data("fd.db.moved-to-postgres")) {
			t.Fatal("내리기가 섰는데 선택 파일 · 표지가 바뀌었다")
		}
	})
	t.Run("계정 검사가 서면 선택 파일을 켜지 않고, 다시 돌면 6 부터 잇는다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		r.env["FDT_WAIT"] = "1"
		rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
		if rc == "0" || strings.Contains(strings.Join(calls, "\n"), "verify") {
			t.Fatalf("db-roles 가 섰는데 덩이가 맞대기로 갔다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		if !r.exists(r.off()) || r.exists(r.on()) || !r.exists(r.data("fd.db.moved-to-postgres")) {
			t.Fatal("db-roles 가 섰는데 선택 파일이 켜졌거나 표지가 없다")
		}
		// 새 셸: 1 이 낸 저장소 마운트 줄만 다시 준다(서버가 내려가 fdwhere 는 못 읽는다).
		r.env["FDT_WAIT"] = "0"
		rc, calls, out = r.run(r.work, "export FD_REPOS='/repos/one' FD_REPOS2='/repos/two' FD_REPOS3='' FD_REPOS4=''", procs.upgrade)
		if rc != "0" {
			t.Fatalf("다시 돈 덩이가 %s 로 섰다:\n%s", rc, out)
		}
		joined := strings.Join(calls, "\n")
		if strings.Contains(joined, " down") || strings.Contains(joined, "import-sqlite") || called(calls, "run --rm --no-deps migrate migrate verify --from /data/fd.db") == "" {
			t.Fatalf("다시 돈 덩이가 2-5 를 건너뛰고 6 부터 돌지 않았다:\n%s", joined)
		}
		if !r.exists(r.on()) || r.exists(r.off()) {
			t.Fatal("다시 돈 덩이가 선택 파일을 켜지 않았다")
		}
	})
	t.Run("옛 자리의 선택 파일이 남았으면 compose 를 안 부른다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		r.write(r.data("compose.env"), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
		rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
		if rc == "0" || len(composeCalls(calls)) != 0 || !strings.Contains(out, "~/.flightdeck/compose.env") {
			t.Fatalf("옛 자리 관문이 안 섰다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
	})
	t.Run("표지 없이 선택 파일이 켜져 있으면 시작하지 않는다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		if err := os.Rename(r.off(), r.on()); err != nil {
			t.Fatal(err)
		}
		rc, calls, _ := r.run(r.work, "fdwhere", procs.upgrade)
		if rc == "0" || len(composeCalls(calls)) != 0 {
			t.Fatalf("켜진 선택 파일 · 표지 없음에서 덩이가 compose 를 불렀다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
	})
}

func checkBeforeSwitchOn(t *testing.T, shell string, procs readmeProcs) {
	stopped := func(t *testing.T) *procRig {
		r := newProcRig(t, shell, procs)
		r.write(r.off(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
		r.write(r.data("fd.db"), "old")
		r.write(r.data("fd.db.moved-to-postgres"), "")
		r.env["FD_REPOS"] = "/repos/one"
		return r
	}
	t.Run("재기: 꺼 둔 파일로 맞댄다", func(t *testing.T) {
		r := stopped(t)
		rc, calls, out := r.run(r.work, "", procs.measure)
		if c := called(calls, "run --rm migrate migrate verify --from /data/fd.db"); rc != "0" || !strings.Contains(c, " ENV="+r.off()+" ") {
			t.Fatalf("재기가 꺼 둔 파일로 맞대지 않았다(rc=%s, %q):\n%s", rc, c, out)
		}
	})
	for _, tc := range []struct {
		name  string
		setup func(r *procRig)
	}{
		{"선택 파일이 켜져 있다", func(r *procRig) { _ = os.Rename(r.off(), r.on()) }},
		{"서버가 돈다", func(r *procRig) { r.serverUp("newp", r.work) }},
	} {
		t.Run("재기가 선다: "+tc.name, func(t *testing.T) {
			r := stopped(t)
			tc.setup(r)
			rc, calls, out := r.run(r.work, "", procs.measure)
			if rc == "0" || len(composeCalls(calls)) != 0 {
				t.Fatalf("재기가 서지 않고 compose 를 불렀다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
			}
		})
	}
	t.Run("돌아가기: 겹을 내리고 표지를 지우고 기본 경로로 띄운다", func(t *testing.T) {
		r := stopped(t)
		rc, calls, out := r.run(r.work, "", procs.back)
		if rc != "0" {
			t.Fatalf("돌아가기가 %s 로 섰다:\n%s", rc, out)
		}
		down, up := called(calls, "down"), called(calls, "up -d --build")
		if !strings.Contains(down, " ENV="+r.off()+" ") || !strings.Contains(up, " ENV= ") {
			t.Fatalf("돌아가기가 꺼 둔 파일로 내리고 기본 경로로 띄우지 않았다:\n%s", strings.Join(calls, "\n"))
		}
		if r.exists(r.data("fd.db.moved-to-postgres")) || !r.exists(r.off()) || r.exists(r.on()) {
			t.Fatal("돌아간 뒤 표지가 남았거나 선택 파일이 바뀌었다")
		}
	})
	t.Run("돌아가기가 선다: 선택 파일이 켜져 있다", func(t *testing.T) {
		r := stopped(t)
		_ = os.Rename(r.off(), r.on())
		rc, calls, _ := r.run(r.work, "", procs.back)
		if rc == "0" || len(composeCalls(calls)) != 0 || !r.exists(r.data("fd.db.moved-to-postgres")) {
			t.Fatalf("켜진 선택 파일에서 돌아가기가 compose 를 불렀거나 표지를 지웠다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
	})
}

// rollbackRig 는 옮긴 뒤의 자리다: 켠 선택 파일, 표지, 옮긴 SQLite 원장, 이 판의 자리(work)에서 도는 Postgres 서버. 절차는 다른
// 자리(old)에서 돌린다 - 덩이가 라벨이 가리키는 자리 · 프로젝트로 도는지 잰다.
func rollbackRig(t *testing.T, shell string, procs readmeProcs) *procRig {
	r := newProcRig(t, shell, procs)
	r.write(r.on(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
	r.write(r.data("fd.db"), "old")
	r.write(r.data("fd.db.moved-to-postgres"), "")
	r.serverUp("newp", r.work)
	return r
}

func checkRollback(t *testing.T, shell string, procs readmeProcs) {
	t.Run("끝까지 돈다", func(t *testing.T) {
		r := rollbackRig(t, shell, procs)
		rc, calls, out := r.run(r.old, "fdwhere", procs.rollback)
		if rc != "0" {
			t.Fatalf("덩이가 %s 로 섰다:\n%s", rc, out)
		}
		for _, c := range composeCalls(calls) {
			if !strings.HasPrefix(c, "PWD="+r.work+" ") || !strings.Contains(c, " ARGS=compose -p newp ") {
				t.Errorf("compose 가 도는 서버의 자리 · 프로젝트로 안 돌았다: %q", c)
			}
		}
		if c := called(calls, "run --rm --no-deps migrate migrate export-sqlite --to /data/fd-back.db"); !strings.Contains(c, " ENV="+r.on()+" ") {
			t.Errorf("역이관이 켠 선택 파일로 안 돌았다: %q", c)
		}
		if c := called(calls, "-p newp up -d"); !strings.Contains(c, " ENV= ") {
			t.Errorf("마지막 up 이 기본 경로(SQLite)로 안 돌았다: %q", c)
		}
		if r.exists(r.on()) || !r.exists(r.off()) || r.exists(r.data("fd.db.moved-to-postgres")) || r.read(r.data("fd.db")) != "back" {
			t.Fatal("끝난 뒤 선택 파일 · 표지 · 원장이 맞지 않다")
		}
		kept, _ := filepath.Glob(r.data("fd.db.pre-back-*"))
		if len(kept) != 1 || r.read(kept[0]) != "old" {
			t.Fatalf("옛 원장이 옆 이름으로 하나 남지 않았다: %v", kept)
		}
	})
	t.Run("앞 실행이 낸 파일이 있으면 시작하지 않는다", func(t *testing.T) {
		r := rollbackRig(t, shell, procs)
		r.write(r.data("fd-back.db"), "stale")
		rc, calls, _ := r.run(r.old, "fdwhere", procs.rollback)
		if rc == "0" || len(composeCalls(calls)) != 0 {
			t.Fatalf("남은 fd-back.db 로 덩이가 compose 를 불렀다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
		if r.read(r.data("fd.db")) != "old" || !r.exists(r.on()) {
			t.Fatal("남은 fd-back.db 가 원장 자리에 놓였거나 선택 파일이 꺼졌다")
		}
	})
	t.Run("손으로 돌린 덤프가 돌면 역이관 전에 선다", func(t *testing.T) {
		r := rollbackRig(t, shell, procs)
		r.env["FDT_DUMPS"] = "newp-db-backup-run-1(once=True)"
		rc, calls, out := r.run(r.old, "fdwhere", procs.rollback)
		if rc == "0" || strings.Contains(strings.Join(calls, "\n"), "export-sqlite") || called(calls, "-p newp down") != "" {
			t.Fatalf("덤프가 도는데 덩이가 역이관 · 내리기로 갔다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
		if !strings.Contains(out, "once=True") || !r.exists(r.on()) || r.read(r.data("fd.db")) != "old" {
			t.Fatalf("덤프를 대지 않았거나 선택 파일 · 원장이 바뀌었다:\n%s", out)
		}
	})
	t.Run("맞대기가 서면 겹을 안 내리고 아무것도 안 놓는다", func(t *testing.T) {
		r := rollbackRig(t, shell, procs)
		r.env["FDT_FAIL"] = "verify --from /data/fd-back.db$"
		rc, calls, _ := r.run(r.old, "fdwhere", procs.rollback)
		if rc == "0" || called(calls, "-p newp down") != "" {
			t.Fatalf("맞대기가 섰는데 덩이가 내리기로 갔다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
		if !r.exists(r.on()) || !r.exists(r.data("fd.db.moved-to-postgres")) || r.read(r.data("fd.db")) != "old" {
			t.Fatal("맞대기가 섰는데 선택 파일 · 표지 · 원장이 바뀌었다")
		}
		// 다시 붙여 넣으면 이번 실행이 낸(맞지 않은) 파일을 보고 선다.
		delete(r.env, "FDT_FAIL")
		if rc, calls, _ := r.run(r.old, "fdwhere", procs.rollback); rc == "0" || len(composeCalls(calls)) != 0 {
			t.Fatalf("맞지 않은 fd-back.db 를 두고 다시 돈 덩이가 compose 를 불렀다(rc=%s)", rc)
		}
	})
	t.Run("4 뒤에 섰으면 다시 붙여 넣어 5 부터 잇는다", func(t *testing.T) {
		r := rollbackRig(t, shell, procs)
		r.env["FDT_FAIL"] = "-p newp up -d$"
		rc, _, _ := r.run(r.old, "fdwhere", procs.rollback)
		if rc == "0" {
			t.Fatal("up 이 섰는데 덩이가 0 으로 끝났다")
		}
		// 5 가 놓았고 6 이 섰다: 다시 붙여 넣으면 그것을 대고 선다(6 만 다시 돈다).
		delete(r.env, "FDT_FAIL")
		prelude := "p=newp w='" + r.work + "'; export FD_REPOS='/repos/one'"
		if rc, calls, _ := r.run(r.old, prelude, procs.rollback); rc == "0" || len(composeCalls(calls)) != 0 || r.read(r.data("fd.db")) != "back" {
			t.Fatalf("놓은 뒤 다시 돈 덩이가 서지 않았거나 원장을 건드렸다(rc=%s)", rc)
		}
	})
	t.Run("4 끝 뒤 5 전에 섰으면 낸 파일을 놓고 띄운다", func(t *testing.T) {
		r := newProcRig(t, shell, procs)
		r.write(r.off(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
		r.write(r.data("fd.db"), "old")
		r.write(r.data("fd-back.db"), "back")
		r.write(r.data("fd.db.moved-to-postgres"), "")
		prelude := "p=newp w='" + r.work + "'; export FD_REPOS='/repos/one'"
		rc, calls, out := r.run(r.old, prelude, procs.rollback)
		if rc != "0" || strings.Contains(strings.Join(calls, "\n"), "export-sqlite") {
			t.Fatalf("4 뒤에서 잇지 않았다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		if r.read(r.data("fd.db")) != "back" || r.exists(r.data("fd.db.moved-to-postgres")) || called(calls, "-p newp up -d") == "" {
			t.Fatal("4 뒤에서 이은 덩이가 낸 파일을 놓고 띄우지 않았다")
		}
	})
	t.Run("선택 파일이 꺼져 있고 낸 파일도 없으면 선다", func(t *testing.T) {
		r := newProcRig(t, shell, procs)
		r.write(r.off(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
		r.write(r.data("fd.db"), "old")
		r.write(r.data("fd.db.moved-to-postgres"), "")
		prelude := "p=newp w='" + r.work + "'; export FD_REPOS='/repos/one'"
		rc, calls, _ := r.run(r.old, prelude, procs.rollback)
		if rc == "0" || len(composeCalls(calls)) != 0 || r.read(r.data("fd.db")) != "old" || !r.exists(r.data("fd.db.moved-to-postgres")) {
			t.Fatalf("켜기 전 배포에서 72시간 되돌리기가 서지 않았다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
	})
}
