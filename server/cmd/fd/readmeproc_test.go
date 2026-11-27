package main

// README 의 Postgres 절차(「업그레이드: SQLite 원장을 Postgres 로」 · 「켜기 전에 섰을 때」 · 「72시간 안에 되돌리기」)와 fd-update
// 스킬의 갱신 사슬을 글자 그대로 떼어, 대역 docker 로 돌리는 시험이다.
//
// ★ 글자가 아니라 **도는 모양**을 잰다. 이 절차들은 한때 단계마다 `&&` 사슬이었다 - 사슬은 그 단계 안에서만 서고, 다음 단계는
//   앞 단계가 섰어도 돌았다(옛 서버를 못 내렸는데 옮기고, 계정 검사가 섰는데 선택 파일을 켠다). 그래서 셸이 실제로 어디서 서는지,
//   선 뒤에 무엇이 남는지(선택 파일 · 이관 표지 · 원장 파일)를 실제 셸로 잰다.
// ★ 셸은 사람이 붙여 넣는 꼴 그대로다: 대화형 zsh(`zsh -f -i`, macOS 의 기본 셸)와 대화형 bash(macOS 에서는 /bin/bash 3.2)에
//   덩이를 표준 입력으로 준다. 비대화형 `-c` 로는 못 보는 것이 있다 - 대화형 zsh 는 `interactivecomments` 가 꺼져 있어 `#` 줄을
//   명령으로 돌리고(`set -e` 덩이가 그 첫 줄에서 선다), zsh · bash 3.2 에는 `readarray` 가 없다.
// ★ 셸 함수(fdc · fdrepos · fdwhere · srvsql · nodump)도 README 의 정의를 그대로 쓴다 - 대역은 docker 하나다. 대역은 부른 것을
//   (자리 · 넘긴 선택 파일 · 저장소 마운트 · 인자) 적고, 상태 디렉토리로 서버 컨테이너를 흉내 내고, 이관 · 역이관이 남기는 파일을
//   만든다. 운영 docker · 운영 자리(/etc/flightdeck · 홈)는 건드리지 않는다: 선택 파일 자리를 시험 자리로 바꿔 쓰고 HOME 을 시험
//   자리로 준다.

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

// readmeBlocks 는 글의 ```bash 블록들이다. 목록 안에 들여 쓴 블록은 그 들여쓰기만큼 걷어 낸다.
func readmeBlocks(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile("(?ms)^([ \t]*)```bash\n(.*?)\n[ \t]*```[ \t]*$").FindAllStringSubmatch(body, -1) {
		lines := strings.Split(m[2], "\n")
		for i, ln := range lines {
			lines[i] = strings.TrimPrefix(ln, m[1])
		}
		out = append(out, strings.Join(lines, "\n"))
	}
	return out
}

func readmeBody(t *testing.T, readme string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pluginRoot(t), readme))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func loadReadmeProcs(t *testing.T, readme string) readmeProcs {
	t.Helper()
	blocks := readmeBlocks(readmeBody(t, readme))
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
	p.rollback = one("72시간 되돌리기 2-7", "export-sqlite --to /data/fd-back.db --dry-run")
	// 두 절차의 1 은 fdwhere 한 줄이다 - 시험의 「처음 돌기」가 그 줄을 그대로 앞에 붙인다.
	n := 0
	for _, b := range blocks {
		if strings.TrimSpace(b) == "fdwhere" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%s 에서 1 단계(fdwhere 한 줄) 블록이 %d 개다 - 업그레이드 · 72시간 되돌리기에 하나씩이다", readme, n)
	}
	return p
}

// fakeDocker 는 대역 docker 다. 부른 것을 FDT_LOG 에 한 줄씩 적는다:
// `PWD=<자리> ENV=<COMPOSE_ENV_FILES> REPOS=<FD_REPOS>,<FD_REPOS2>,<FD_REPOS3>,<FD_REPOS4> ARGS=<인자>`.
// FDT_FAIL(정규식)에 맞는 인자면 1 로 끝난다. FDT_STATE/created 가 있으면 서버 컨테이너(flightdeck)와 db 컨테이너가 있고,
// FDT_STATE/up 이 있으면 서버가 돈다(stop 은 멈추기만, down 은 지운다). FDT_STATE/started 는 서버가 한 번이라도 돌았다는 것이다
// (없으면 `.State.Status` 가 created - 만들기만 하고 못 돌았다). FDT_STATE/hash 는 그 컨테이너들을 만든 설정의 config-hash 다(up 이
// 그때의 FDT_HASH 로 적는다, 기본 h1). `config --hash` 는 지금의 FDT_HASH 를 낸다. FDT_UP_CREATE_ONLY 가 있으면 up 은 컨테이너를
// 만들기만 하고 실패한다(db · migrate 에서 섰다). FDT_MOUNTS 가 있으면 서버의 저장소 마운트는 그 줄들이다(기본 /repos/one ·
// /repos/two). FDT_NAMES 는 다른 프로젝트에 남은 컨테이너 이름이다. FDT_STATE/pgrows 가 있으면 Postgres 원장에 행이 있다 - 이관의
// 예행이 「대상이 비어 있지 않다」로 선다(본 이관이 커밋하며 만든다).
const fakeDocker = `#!/bin/sh
printf 'PWD=%s ENV=%s REPOS=%s,%s,%s,%s ARGS=%s\n' "$PWD" "${COMPOSE_ENV_FILES-}" "${FD_REPOS-}" "${FD_REPOS2-}" "${FD_REPOS3-}" "${FD_REPOS4-}" "$*" >> "$FDT_LOG"
if [ -n "${FDT_FAIL-}" ] && printf '%s' "$*" | grep -Eq -- "$FDT_FAIL"; then echo "대역 docker: 실패를 낸다($*)" >&2; exit 1; fi
st=$FDT_STATE
case "$1" in
compose)
  case " $* " in
  *" import-sqlite "*" --dry-run "*)
    if [ -e "$st/pgrows" ]; then echo "대상이 비어 있지 않다(item 3 행) - 이관은 빈 원장에만 쓴다" >&2; exit 1; fi ;;
  *" export-sqlite "*" --dry-run "*) ;;
  *" import-sqlite "*) : > "$st/pgrows"; : > "$HOME/.flightdeck/fd.db.moved-to-postgres" ;;
  *" export-sqlite "*) [ ! -e "$HOME/.flightdeck/fd-back.db" ] || exit 1; echo back > "$HOME/.flightdeck/fd-back.db" ;;
  *" ps -aq db-roles "*) echo rolesid ;;
  *" ps -aq db "*) [ ! -e "$st/created" ] || echo dbid ;;
  *" ps -aq flightdeck "*) [ ! -e "$st/created" ] || echo srvid ;;
  *" config --hash "*) for a in "$@"; do last=$a; done; echo "$last ${FDT_HASH:-h1}" ;;
  *" stop flightdeck "*) rm -f "$st/up" ;;
  *" down "*) rm -f "$st/up" "$st/created" "$st/started" "$st/hash" ;;
  esac
  case "$*" in *" up -d"|*" up -d --build")
    : > "$st/created"; echo "${FDT_HASH:-h1}" > "$st/hash"
    if [ -n "${FDT_UP_CREATE_ONLY-}" ]; then echo "대역 docker: 만들기만 하고 섰다" >&2; exit 1; fi
    : > "$st/up"; : > "$st/started" ;;
  esac
  exit 0 ;;
inspect)
  [ -e "$st/created" ] || { echo "Error: No such object: flightdeck" >&2; exit 1; }
  case "$*" in
  *working_dir*) echo "$FDT_W" ;;
  *compose.project*) echo "$FDT_P" ;;
  *config-hash*) cat "$st/hash" ;;
  *State.Status*) if [ -e "$st/up" ]; then echo running; elif [ -e "$st/started" ]; then echo exited; else echo created; fi ;;
  *Mounts*) if [ -n "${FDT_MOUNTS+x}" ]; then printf '%s\n' "$FDT_MOUNTS"; else printf '%s\n' /repos/one /repos/two; fi ;;
  *Config.Env*) echo "FD_DB=${FDT_FD_DB:-/data/fd.db}" ;;
  esac
  exit 0 ;;
ps)
  case "$*" in
  *service=flightdeck*) [ ! -e "$st/up" ] || echo srvid ;;
  *service=db-backup*) [ -z "${FDT_DUMPS-}" ] || echo "$FDT_DUMPS" ;;
  *" --format {{.Names}}") [ ! -e "$st/created" ] || echo flightdeck; [ -z "${FDT_NAMES-}" ] || echo "$FDT_NAMES" ;;
  esac
  exit 0 ;;
wait) echo "${FDT_WAIT:-0}"; exit 0 ;;
esac
exit 0
`

// procShell 은 덩이를 붙여 넣는 셸 하나다. stdin 이면 덩이를 표준 입력으로 준다(대화형 셸에 붙여 넣는 꼴), 아니면 `-c` 로 준다.
type procShell struct {
	name  string
	path  string
	args  []string
	stdin bool
}

// pasteShells 는 사람이 덩이를 붙여 넣는 셸이다 - 대화형 bash(macOS 면 /bin/bash 3.2)와, 있으면 대화형 zsh.
func pasteShells(t *testing.T) []procShell {
	t.Helper()
	bash := "/bin/bash"
	if _, err := os.Stat(bash); err != nil {
		p, err := exec.LookPath("bash")
		if err != nil {
			t.Fatalf("bash 가 없다 - README 절차를 셸로 재지 못한다: %v", err)
		}
		bash = p
	}
	out := []procShell{{"bash -i", bash, []string{"--norc", "--noprofile", "--noediting", "-i"}, true}}
	if zsh, err := exec.LookPath("zsh"); err == nil {
		out = append(out, procShell{"zsh -f -i", zsh, []string{"-f", "-i"}, true})
	}
	return out
}

// procRig 는 시험 한 판의 자리다: 홈(~/.flightdeck 가 /data 다) · 선택 파일 자리 · 대역 docker · 상태 · 기록.
type procRig struct {
	t                          *testing.T
	shell                      procShell
	procs                      readmeProcs
	home, etc, state, log, bin string
	work, old                  string // 이 판의 자리(절차를 돌리는 곳) · 도는 서버의 라벨이 가리키는 자리
	env                        map[string]string
}

func newProcRig(t *testing.T, shell procShell, procs readmeProcs) *procRig {
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
	r.write(filepath.Join(r.state, "created"), "")
	r.write(filepath.Join(r.state, "up"), "")
	r.env["FDT_P"], r.env["FDT_W"] = p, w
}

// sub 는 글의 운영 자리(/etc/flightdeck)를 이 판의 자리로 바꾼다.
func (r *procRig) sub(s string) string { return strings.ReplaceAll(s, "/etc/flightdeck", r.etc) }

// exec 는 cwd 에서 스크립트를 이 판의 셸로 돌리고 출력과 docker 기록 줄을 낸다. 종료 코드는 스크립트가 `BLOCK_RC=` 로 찍는다
// (대화형 셸은 표준 입력이 끝나면 마지막 명령의 코드로 끝나 덩이의 코드를 잃는다).
func (r *procRig) exec(cwd, script string) (rc string, calls []string, out string) {
	r.t.Helper()
	_ = os.Remove(r.log)
	var cmd *exec.Cmd
	if r.shell.stdin {
		cmd = exec.Command(r.shell.path, r.shell.args...)
		cmd.Stdin = strings.NewReader(script + "\n")
	} else {
		cmd = exec.Command(r.shell.path, append(append([]string{}, r.shell.args...), "-c", script)...)
	}
	cmd.Dir = cwd
	env := []string{"HOME=" + r.home, "PATH=" + r.bin + ":/usr/bin:/bin:/usr/sbin:/sbin", "LANG=C.UTF-8",
		"FDT_LOG=" + r.log, "FDT_STATE=" + r.state, "PS1=", "PS2=", "BASH_SILENCE_DEPRECATION_WARNING=1"}
	for k, v := range r.env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	b, _ := cmd.CombinedOutput()
	out = string(b)
	if m := regexp.MustCompile(`BLOCK_RC=(\d+)`).FindStringSubmatch(out); m != nil {
		rc = m[1]
	} else {
		r.t.Fatalf("%s: 덩이의 종료 코드가 안 나왔다(셸이 덩이 전에 섰다):\n%s", r.shell.name, out)
	}
	if raw, err := os.ReadFile(r.log); err == nil {
		calls = strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
	return rc, calls, out
}

// run 은 셸 함수 정의 뒤 cwd 에서 앞줄(prelude)과 덩이(block)를 돌린다. 덩이의 부분 셸이 닫힌 바로 뒤에 그 종료 코드를 찍어
// 낸다(덩이 뒤의 curl 같은 줄이 코드를 덮지 않게).
func (r *procRig) run(cwd, prelude, block string) (rc string, calls []string, out string) {
	r.t.Helper()
	body := r.sub(block) + "\n"
	if !strings.Contains(body, "\n)\n") {
		r.t.Fatalf("덩이가 `)` 줄로 닫히는 부분 셸이 아니다:\n%s", block)
	}
	body = strings.Replace(body, "\n)\n", "\n)\necho \"BLOCK_RC=$?\"\n", 1)
	return r.exec(cwd, r.sub(r.procs.defs)+"\ncd '"+cwd+"' || exit 99\n"+prelude+"\n"+body)
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

// calledAt 은 compose 호출 가운데 인자가 args 로 끝나는 첫 줄의 차례다(없으면 -1).
func calledAt(calls []string, args string) int {
	for i, c := range composeCalls(calls) {
		if strings.HasSuffix(c, " "+args) {
			return i
		}
	}
	return -1
}

// ran 은 기록 줄이 자리 pwd · 선택 파일 env 로 돌았는지다.
func ran(c, pwd, env string) bool { return strings.HasPrefix(c, "PWD="+pwd+" ENV="+env+" REPOS=") }

// TestREADMEProceduresStopWhole 은 README(정본 · 번역본)의 세 절차가 첫 실패에서 통째로 서고, 선 자리에 맞는 것만 남기며, 다시
// 붙여 넣으면 된 단계를 건너뛰는지 잰다 - 사람이 붙여 넣는 대화형 셸로.
func TestREADMEProceduresStopWhole(t *testing.T) {
	for _, readme := range []string{"README.ko.md", "README.md"} {
		procs := loadReadmeProcs(t, readme)
		for _, shell := range pasteShells(t) {
			t.Run(readme+"/"+shell.name, func(t *testing.T) {
				t.Parallel()
				t.Run("업그레이드", func(t *testing.T) { checkUpgrade(t, shell, procs) })
				t.Run("켜기 전에 섰을 때", func(t *testing.T) { checkBeforeSwitchOn(t, shell, procs) })
				t.Run("72시간 되돌리기", func(t *testing.T) { checkRollback(t, shell, procs) })
			})
		}
	}
}

// upgradeRig 는 업그레이드 전의 자리다: 꺼 둔 선택 파일, SQLite 원장, 옛 판 자리에서 도는 서버.
func upgradeRig(t *testing.T, shell procShell, procs readmeProcs) *procRig {
	r := newProcRig(t, shell, procs)
	r.write(r.off(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
	r.write(r.data("fd.db"), "old")
	r.serverUp("oldp", r.old)
	return r
}

// switchedOnRig 는 업그레이드가 7 에서 선택 파일을 켠 뒤 8 전에 선 자리다: 켠 선택 파일, 표지, Postgres 원장의 행, 내려간 서버.
func switchedOnRig(t *testing.T, shell procShell, procs readmeProcs) *procRig {
	r := newProcRig(t, shell, procs)
	r.write(r.on(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
	r.write(r.data("fd.db"), "old")
	r.write(r.data("fd.db.moved-to-postgres"), "")
	r.write(filepath.Join(r.state, "pgrows"), "")
	return r
}

const newShellRepos = "export FD_REPOS='/repos/one' FD_REPOS2='/repos/two' FD_REPOS3='' FD_REPOS4=''"

func checkUpgrade(t *testing.T, shell procShell, procs readmeProcs) {
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
			{"run --rm migrate migrate verify --from /data/fd.db", r.work, r.off()},
			{"up -d --build", r.work, r.on()},
		} {
			if c := called(calls, want.args); !ran(c, want.pwd, want.env) {
				t.Errorf("`%s` 가 자리 %s · 선택 파일 %q 로 안 돌았다: %q", want.args, want.pwd, want.env, c)
			}
		}
		if c := called(calls, "up -d --build"); !strings.Contains(c, " REPOS=/repos/one,/repos/two,, ") {
			t.Errorf("8 의 up 이 1 이 읽은 저장소 마운트로 안 돌았다: %q", c)
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
		rc, calls, out = r.run(r.work, newShellRepos, procs.upgrade)
		if rc != "0" {
			t.Fatalf("다시 돈 덩이가 %s 로 섰다:\n%s", rc, out)
		}
		joined := strings.Join(calls, "\n")
		if strings.Contains(joined, " down") || strings.Contains(joined, "import-sqlite") || called(calls, "run --rm migrate migrate verify --from /data/fd.db") == "" {
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
	// 어느 원장(볼륨 · DB)인지는 선택 파일에만 있다 - 셸이 주면(빈 값도) 새 셸에서 빠져 다른 볼륨을 고를 수 있어 관문이 선다.
	for _, kv := range [][2]string{{"FD_PG_INSTANCE", "other"}, {"FD_PG_DATABASE", ""}} {
		t.Run("셸이 "+kv[0]+" 를 주면 compose 를 안 부른다", func(t *testing.T) {
			r := upgradeRig(t, shell, procs)
			r.env[kv[0]] = kv[1]
			rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
			if rc == "0" || len(composeCalls(calls)) != 0 || !strings.Contains(out, "unset FD_PG_INSTANCE FD_PG_DATABASE") {
				t.Fatalf("셸의 %s=%q 에 관문이 안 섰다(rc=%s):\n%s\n%s", kv[0], kv[1], rc, strings.Join(calls, "\n"), out)
			}
			if !r.exists(r.off()) || r.exists(r.data("fd.db.moved-to-postgres")) {
				t.Fatal("관문이 섰는데 선택 파일 · 표지가 바뀌었다")
			}
		})
	}
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
	t.Run("켠 뒤 새 셸에서 이으면 띄우기 전에 선택 파일의 원장을 다시 맞댄다", func(t *testing.T) {
		r := switchedOnRig(t, shell, procs)
		rc, calls, out := r.run(r.work, newShellRepos, procs.upgrade)
		if rc != "0" {
			t.Fatalf("켠 뒤 이은 덩이가 %s 로 섰다:\n%s", rc, out)
		}
		v, u := calledAt(calls, "run --rm migrate migrate verify --from /data/fd.db"), calledAt(calls, "up -d --build")
		if v < 0 || u < 0 || v > u || !ran(composeCalls(calls)[v], r.work, r.on()) {
			t.Fatalf("켠 뒤 이은 덩이가 켠 선택 파일로 맞댄 뒤에 띄우지 않았다:\n%s", strings.Join(calls, "\n"))
		}
		joined := strings.Join(calls, "\n")
		if strings.Contains(joined, " down") || strings.Contains(joined, "import-sqlite") || strings.Contains(joined, "db-roles") {
			t.Fatalf("켠 뒤 이은 덩이가 2-6 을 다시 돌았다:\n%s", joined)
		}
	})
	t.Run("켠 뒤 맞대기가 서면 띄우지 않는다", func(t *testing.T) {
		r := switchedOnRig(t, shell, procs)
		r.env["FDT_FAIL"] = "verify --from /data/fd.db$"
		rc, calls, _ := r.run(r.work, newShellRepos, procs.upgrade)
		if rc == "0" || called(calls, "up -d --build") != "" {
			t.Fatalf("맞대기가 섰는데 덩이가 띄웠다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
	})
	// ★ 서버 컨테이너가 **있다**는 것만으로 7 의 맞대기를 건너뛰지 않는다. `ps -aq` 는 만들기만 하고 못 돈 컨테이너도 낸다 - 8 의 up
	//   이 db · migrate 에서 서면 서버 컨테이너는 created 로 남고, 원장에 쓴 적이 없다.
	t.Run("8 의 up 이 만들기만 하고 섰으면 다시 돌 때 맞댄 뒤에 띄운다", func(t *testing.T) {
		r := switchedOnRig(t, shell, procs)
		r.env["FDT_UP_CREATE_ONLY"] = "1"
		if rc, calls, out := r.run(r.work, newShellRepos, procs.upgrade); rc == "0" || called(calls, "run --rm migrate migrate verify --from /data/fd.db") == "" {
			t.Fatalf("만들기만 한 up 에서 덩이가 0 으로 끝났거나 맞대지 않았다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		if !r.exists(filepath.Join(r.state, "created")) || r.exists(filepath.Join(r.state, "started")) {
			t.Fatal("대역이 만들기만 한 서버 컨테이너를 남기지 않았다 - 시험이 그 자리를 못 잰다")
		}
		delete(r.env, "FDT_UP_CREATE_ONLY")
		rc, calls, out := r.run(r.work, newShellRepos, procs.upgrade)
		v, u := calledAt(calls, "run --rm migrate migrate verify --from /data/fd.db"), calledAt(calls, "up -d --build")
		if rc != "0" || v < 0 || u < 0 || v > u {
			t.Fatalf("만들기만 한 서버 컨테이너가 있는 자리에서 다시 돈 덩이가 맞댄 뒤에 띄우지 않았다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
	})
	served := func(r *procRig, hash string) {
		for _, n := range []string{"created", "started"} {
			r.write(filepath.Join(r.state, n), "")
		}
		r.write(filepath.Join(r.state, "hash"), hash+"\n")
	}
	t.Run("이 자리의 서버가 이미 돌았고 설정이 같으면 맞대지 않고 8 만 돈다", func(t *testing.T) {
		r := switchedOnRig(t, shell, procs)
		served(r, "h1")
		rc, calls, out := r.run(r.work, newShellRepos, procs.upgrade)
		if rc != "0" || strings.Contains(strings.Join(calls, "\n"), "verify") || called(calls, "up -d --build") == "" {
			t.Fatalf("돈 서버가 있는 자리에서 덩이가 맞댔거나 띄우지 않았다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		for _, svc := range []string{"db", "flightdeck"} {
			if c := called(calls, "config --hash "+svc); !ran(c, r.work, r.on()) {
				t.Errorf("7 이 켠 선택 파일로 %s 의 설정을 렌더해 맞대지 않았다: %q", svc, c)
			}
		}
	})
	t.Run("이 자리에서 돈 서버의 설정이 지금과 다르면 띄우지 않는다", func(t *testing.T) {
		r := switchedOnRig(t, shell, procs)
		served(r, "h1")
		r.env["FDT_HASH"] = "h2"
		rc, calls, out := r.run(r.work, newShellRepos, procs.upgrade)
		if rc == "0" || called(calls, "up -d --build") != "" || strings.Contains(strings.Join(calls, "\n"), "verify") {
			t.Fatalf("만든 설정과 지금 설정이 다른데 덩이가 띄웠거나 맞대기로 갔다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		if !strings.Contains(out, "config-hash") && !strings.Contains(out, "settings that created") && !strings.Contains(out, "만든 설정") {
			t.Fatalf("설정이 다르다는 사유를 대지 않았다:\n%s", out)
		}
	})
	t.Run("서버 컨테이너의 상태를 못 읽으면 띄우지 않는다", func(t *testing.T) {
		r := switchedOnRig(t, shell, procs)
		served(r, "h1")
		r.env["FDT_FAIL"] = "State.Status"
		rc, calls, out := r.run(r.work, newShellRepos, procs.upgrade)
		if rc == "0" || called(calls, "up -d --build") != "" {
			t.Fatalf("상태 조회가 섰는데 덩이가 띄웠다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
	})
	t.Run("5 의 옮기기가 서면 이 기계의 표지 자리를 댄다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		r.env["FDT_FAIL"] = "import-sqlite --from /data/fd.db$"
		rc, _, out := r.run(r.work, "fdwhere", procs.upgrade)
		if rc == "0" || !strings.Contains(out, "touch '"+r.data("fd.db.moved-to-postgres")+"'") {
			t.Fatalf("옮기기가 선 자리에서 덩이가 이 기계의 표지 자리(%s)를 대지 않았다(rc=%s):\n%s", r.data("fd.db.moved-to-postgres"), rc, out)
		}
	})
	// 2 의 남은 컨테이너 조회가 실패해도 「안 남았다」로 읽지 않는다(빈 출력은 docker 가 섰다는 뜻일 수 있다).
	for _, fail := range []string{"^ps -aq --filter label=com.docker.compose.project=oldp$", "^ps -a --format"} {
		t.Run("2 의 조회가 실패하면 짓지도 옮기지도 않는다: "+fail, func(t *testing.T) {
			r := upgradeRig(t, shell, procs)
			r.env["FDT_FAIL"] = fail
			rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
			if rc == "0" || called(calls, "build") != "" || strings.Contains(strings.Join(calls, "\n"), "import-sqlite") {
				t.Fatalf("2 의 조회가 섰는데 덩이가 이어 갔다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
			}
		})
	}
	t.Run("다른 프로젝트에 서버 컨테이너가 남았으면 짓지 않는다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		r.env["FDT_NAMES"] = "flightdeck"
		rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
		if rc == "0" || called(calls, "build") != "" || !strings.Contains(out, "(flightdeck)") {
			t.Fatalf("남은 서버 컨테이너에 덩이가 서지 않았다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		r = upgradeRig(t, shell, procs)
		r.env["FDT_NAMES"] = "flightdeck-db"
		if rc, _, out := r.run(r.work, "fdwhere", procs.upgrade); rc != "0" {
			t.Fatalf("이름이 flightdeck 으로 시작할 뿐인 컨테이너에 덩이가 섰다(rc=%s):\n%s", rc, out)
		}
	})
	// 공백이 든 저장소 경로는 한 경로다 - 1 이 읽은 그대로 8 의 up 에 넘어간다.
	t.Run("공백이 든 저장소 마운트가 8 에 그대로 넘어간다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		r.env["FDT_MOUNTS"] = "/repos/team one\n/repos/two"
		rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
		if c := called(calls, "up -d --build"); rc != "0" || !strings.Contains(c, " REPOS=/repos/team one,/repos/two,, ") {
			t.Fatalf("공백 경로가 8 의 up 에 한 경로로 안 넘어갔다(rc=%s): %q\n%s", rc, c, out)
		}
	})
	t.Run("저장소 마운트가 넷을 넘으면 1 이 서고 아무것도 안 부른다", func(t *testing.T) {
		r := upgradeRig(t, shell, procs)
		r.env["FDT_MOUNTS"] = "/r/1\n/r/2\n/r/3\n/r/4\n/r/5"
		rc, calls, out := r.run(r.work, "fdwhere", procs.upgrade)
		if rc == "0" || len(composeCalls(calls)) != 0 || !strings.Contains(out, "다섯째부터") {
			t.Fatalf("마운트 다섯에 1 이 서지 않았다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
	})
	t.Run("결과를 모르는 5 를 다시 붙여 넣어도 두 번 옮기지 않고 가 를 댄다", func(t *testing.T) {
		// 앞 실행의 5 가 커밋한 직후 끊겼다: Postgres 원장에 행이 있고 표지는 없다. 서버는 앞 실행이 내렸다 - 새 셸에서 1 의 끝 두
		// 줄을 다시 준다.
		r := newProcRig(t, shell, procs)
		r.write(r.off(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
		r.write(r.data("fd.db"), "old")
		r.write(filepath.Join(r.state, "pgrows"), "")
		rc, calls, out := r.run(r.work, newShellRepos+"; p=oldp w='"+r.old+"'", procs.upgrade)
		if rc == "0" || called(calls, "run --rm --no-deps migrate migrate import-sqlite --from /data/fd.db") != "" {
			t.Fatalf("행이 있는 원장에 덩이가 다시 옮겼다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
		if !strings.Contains(out, "대상이 비어 있지 않다") || !(strings.Contains(out, "「켜기 전에 섰을 때」의 가") || strings.Contains(out, `A of "Stopped before switching on"`)) {
			t.Fatalf("예행이 선 자리에서 덩이가 「켜기 전에 섰을 때」의 가 를 대지 않았다:\n%s", out)
		}
		if r.exists(r.data("fd.db.moved-to-postgres")) || r.exists(r.on()) {
			t.Fatal("예행이 섰는데 표지가 생겼거나 선택 파일이 켜졌다")
		}
	})
	// ★ fdwhere 는 조회(라벨 · 마운트)가 **모두** 성공한 뒤에야 p · w · FD_REPOS … 를 둔다 - 하나만 서도 셋 다 빈 채다. 서버가 없을
	//   때만이 아니라 서버가 도는데 조회 하나가 실패할 때도 잰다(마운트를 먼저 읽고 라벨에서 서면 FD_REPOS 가 남았다).
	for _, tc := range []struct{ name, fail string }{{"서버가 없다", ""}, {"라벨 조회가 실패한다", "Labels"}, {"마운트 조회가 실패한다", "Mounts"}} {
		t.Run("fdwhere 가 못 읽으면 앞서 읽은 값을 비우고 덩이가 선다: "+tc.name, func(t *testing.T) {
			r := upgradeRig(t, shell, procs)
			if tc.fail == "" {
				_ = os.Remove(filepath.Join(r.state, "created"))
				_ = os.Remove(filepath.Join(r.state, "up"))
			} else {
				r.env["FDT_FAIL"] = tc.fail
			}
			stale := "p=stale w='" + r.old + "'; export FD_REPOS=/stale FD_REPOS2=/stale2; fdwhere; " +
				`echo "AFTER p=${p-none} w=${w-none} R=${FD_REPOS-none},${FD_REPOS2-none}"`
			rc, calls, out := r.run(r.work, stale, procs.upgrade)
			if !strings.Contains(out, "AFTER p=none w=none R=none,none") {
				t.Fatalf("못 읽은 fdwhere 가 앞서 읽은 값을 남겼다:\n%s", out)
			}
			if rc == "0" || len(composeCalls(calls)) != 0 {
				t.Fatalf("fdwhere 가 못 읽었는데 덩이가 compose 를 불렀다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
			}
		})
	}
}

func checkBeforeSwitchOn(t *testing.T, shell procShell, procs readmeProcs) {
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
func rollbackRig(t *testing.T, shell procShell, procs readmeProcs) *procRig {
	r := newProcRig(t, shell, procs)
	r.write(r.on(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
	r.write(r.data("fd.db"), "old")
	r.write(r.data("fd.db.moved-to-postgres"), "")
	r.serverUp("newp", r.work)
	return r
}

// offRig 는 되돌리기가 5 에서 선택 파일을 끈 뒤 선 자리다: 꺼 둔 선택 파일, 표지, 옮긴 SQLite 원장, 남은 fd-back.db(내용 back),
// 내려간 서버.
func offRig(t *testing.T, shell procShell, procs readmeProcs) *procRig {
	r := newProcRig(t, shell, procs)
	r.write(r.off(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
	r.write(r.data("fd.db"), "old")
	r.write(r.data("fd-back.db"), "back")
	r.write(r.data("fd.db.moved-to-postgres"), "")
	return r
}

func checkRollback(t *testing.T, shell procShell, procs readmeProcs) {
	resume := func(r *procRig) string { return "p=newp w='" + r.work + "'; export FD_REPOS='/repos/one'" }
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
		for _, want := range []struct{ args, env string }{
			{"-p newp run --rm --no-deps migrate migrate export-sqlite --to /data/fd-back.db", r.on()},
			{"-p newp run --rm migrate migrate verify --from /data/fd-back.db", r.on()},
			{"-p newp down", r.off()},
			{"-p newp up -d --build", ""},
		} {
			if c := called(calls, want.args); !ran(c, r.work, want.env) {
				t.Errorf("`%s` 가 선택 파일 %q 로 안 돌았다: %q", want.args, want.env, c)
			}
		}
		if calledAt(calls, "-p newp run --rm migrate migrate verify --from /data/fd-back.db") > calledAt(calls, "-p newp down") {
			t.Error("맞대기 전에 겹을 내렸다")
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
	t.Run("맞대기가 서면 선택 파일을 안 끄고 아무것도 안 놓는다", func(t *testing.T) {
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
		if rc, calls, _ := r.run(r.old, resume(r), procs.rollback); rc == "0" || len(composeCalls(calls)) != 0 {
			t.Fatalf("맞지 않은 fd-back.db 를 두고 다시 돈 덩이가 compose 를 불렀다(rc=%s)", rc)
		}
	})
	t.Run("5 가 끈 뒤 섰으면 다시 붙여 넣어도 놓기 전에 다시 맞댄다", func(t *testing.T) {
		r := rollbackRig(t, shell, procs)
		r.env["FDT_FAIL"] = "-p newp down$"
		if rc, _, _ := r.run(r.old, "fdwhere", procs.rollback); rc == "0" || r.exists(r.on()) || r.read(r.data("fd.db")) != "old" {
			t.Fatalf("5 의 내리기가 섰는데 덩이가 0 으로 끝났거나(rc=%s) 선택 파일을 안 껐거나 원장을 놓았다", rc)
		}
		delete(r.env, "FDT_FAIL")
		rc, calls, out := r.run(r.old, resume(r), procs.rollback)
		if rc != "0" {
			t.Fatalf("다시 붙여 넣은 덩이가 %s 로 섰다:\n%s", rc, out)
		}
		v, d := calledAt(calls, "-p newp run --rm migrate migrate verify --from /data/fd-back.db"), calledAt(calls, "-p newp down")
		if v < 0 || d < 0 || v > d || !ran(composeCalls(calls)[v], r.work, r.off()) || strings.Contains(strings.Join(calls, "\n"), "export-sqlite") {
			t.Fatalf("다시 붙여 넣은 덩이가 꺼 둔 선택 파일로 다시 맞댄 뒤에 놓지 않았다:\n%s", strings.Join(calls, "\n"))
		}
		if r.read(r.data("fd.db")) != "back" || r.exists(r.data("fd.db.moved-to-postgres")) || called(calls, "-p newp up -d --build") == "" {
			t.Fatal("다시 붙여 넣은 덩이가 낸 파일을 놓고 띄우지 않았다")
		}
	})
	t.Run("5 뒤에서 이을 때 맞대기가 서면 아무것도 안 놓는다", func(t *testing.T) {
		r := offRig(t, shell, procs)
		r.env["FDT_FAIL"] = "verify --from /data/fd-back.db$"
		rc, calls, _ := r.run(r.old, resume(r), procs.rollback)
		if rc == "0" || called(calls, "-p newp down") != "" || called(calls, "-p newp up -d --build") != "" {
			t.Fatalf("맞대기가 섰는데 덩이가 내리기 · 띄우기로 갔다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
		if r.read(r.data("fd.db")) != "old" || r.read(r.data("fd-back.db")) != "back" || !r.exists(r.data("fd.db.moved-to-postgres")) {
			t.Fatal("맞대기가 섰는데 원장 · 낸 파일 · 표지가 바뀌었다 - 낡은 원장이 놓였다")
		}
	})
	t.Run("5 뒤에서 이을 때 서버가 돌면 선다", func(t *testing.T) {
		r := offRig(t, shell, procs)
		r.serverUp("newp", r.work)
		rc, calls, _ := r.run(r.old, resume(r), procs.rollback)
		if rc == "0" || len(composeCalls(calls)) != 0 || r.read(r.data("fd.db")) != "old" {
			t.Fatalf("서버가 도는데 덩이가 맞대기 · 놓기로 갔다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
	})
	t.Run("6 이 놓은 뒤 7 에서 섰으면 7 만 다시 돌라고 대고 선다", func(t *testing.T) {
		r := rollbackRig(t, shell, procs)
		r.env["FDT_FAIL"] = "-p newp up -d --build$"
		if rc, _, _ := r.run(r.old, "fdwhere", procs.rollback); rc == "0" {
			t.Fatal("up 이 섰는데 덩이가 0 으로 끝났다")
		}
		delete(r.env, "FDT_FAIL")
		rc, calls, out := r.run(r.old, resume(r), procs.rollback)
		if rc == "0" || len(composeCalls(calls)) != 0 || r.read(r.data("fd.db")) != "back" || !strings.Contains(out, `fdc -p "newp" up -d --build)`) {
			t.Fatalf("놓은 뒤 다시 돈 덩이가 서지 않았거나 원장을 건드렸다(rc=%s):\n%s", rc, out)
		}
	})
	t.Run("꺼진 선택 파일과 낸 파일로 이으면 다시 맞대고 놓고 띄운다", func(t *testing.T) {
		r := offRig(t, shell, procs)
		rc, calls, out := r.run(r.old, resume(r), procs.rollback)
		if rc != "0" || strings.Contains(strings.Join(calls, "\n"), "export-sqlite") {
			t.Fatalf("5 뒤에서 잇지 않았다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		if c := called(calls, "-p newp run --rm migrate migrate verify --from /data/fd-back.db"); !ran(c, r.work, r.off()) {
			t.Fatalf("5 뒤에서 이은 덩이가 꺼 둔 선택 파일로 맞대지 않았다: %q", c)
		}
		if r.read(r.data("fd.db")) != "back" || r.exists(r.data("fd.db.moved-to-postgres")) || !ran(called(calls, "-p newp up -d --build"), r.work, "") {
			t.Fatal("5 뒤에서 이은 덩이가 낸 파일을 놓고 기본 경로로 띄우지 않았다")
		}
	})
	// ★ docker 조회가 실패하면 빈 출력을 「안 돈다 · 없다」로 읽지 않는다 - 4 의 서버 조회 · 2 의 덤프 조회가 그 자리에서 선다.
	t.Run("4 의 서버 조회가 실패하면 맞대지도 놓지도 않는다", func(t *testing.T) {
		r := offRig(t, shell, procs)
		r.env["FDT_FAIL"] = "^ps -q --filter label=com.docker.compose.service=flightdeck$"
		rc, calls, out := r.run(r.old, resume(r), procs.rollback)
		if rc == "0" || len(composeCalls(calls)) != 0 || r.read(r.data("fd.db")) != "old" || !r.exists(r.data("fd.db.moved-to-postgres")) {
			t.Fatalf("서버 조회가 섰는데 덩이가 맞대기 · 놓기로 갔다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		if !strings.Contains(strings.Join(calls, "\n"), " ARGS=ps -q --filter label=com.docker.compose.service=flightdeck") {
			t.Fatal("대역이 서버 조회를 받지 않았다 - 시험이 그 자리를 못 잰다")
		}
	})
	t.Run("덤프 조회가 실패하면 역이관 전에 선다", func(t *testing.T) {
		r := rollbackRig(t, shell, procs)
		r.env["FDT_FAIL"] = "^ps --filter label=com.docker.compose.service=db-backup "
		rc, calls, out := r.run(r.old, "fdwhere", procs.rollback)
		if rc == "0" || strings.Contains(strings.Join(calls, "\n"), "export-sqlite") || called(calls, "-p newp down") != "" {
			t.Fatalf("덤프 조회가 섰는데 덩이가 역이관 · 내리기로 갔다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
		}
		if !strings.Contains(out, "docker ps 가 실패했다") && !strings.Contains(out, "docker ps failed") {
			t.Fatalf("nodump 가 docker ps 의 실패를 대지 않았다:\n%s", out)
		}
	})
	t.Run("선택 파일이 꺼져 있고 낸 파일도 없으면 선다", func(t *testing.T) {
		r := newProcRig(t, shell, procs)
		r.write(r.off(), "COMPOSE_FILE=compose.yaml:compose.postgres.yaml\n")
		r.write(r.data("fd.db"), "old")
		r.write(r.data("fd.db.moved-to-postgres"), "")
		rc, calls, _ := r.run(r.old, resume(r), procs.rollback)
		if rc == "0" || len(composeCalls(calls)) != 0 || r.read(r.data("fd.db")) != "old" || !r.exists(r.data("fd.db.moved-to-postgres")) {
			t.Fatalf("켜기 전 배포에서 72시간 되돌리기가 서지 않았다(rc=%s):\n%s", rc, strings.Join(calls, "\n"))
		}
	})
}

// pasteSections 는 사람이 블록을 그대로 터미널에 붙여 넣는 README 절이다 - 「빠른 시작」 과 「Postgres 원장」. min 은 그 절에서
// 찾아야 할 bash 블록의 최소 개수다(절을 잘못 자르면 블록을 몇 개 못 찾고 조용히 초록이 된다).
var pasteSections = []struct {
	readme, head string
	min          int
}{
	{"README.ko.md", "\n## 빠른 시작\n", 1}, {"README.ko.md", "\n## Postgres 원장\n", 12},
	{"README.md", "\n## Quick start\n", 1}, {"README.md", "\n## Postgres ledger\n", 12},
}

// pasteSection 은 README 의 그 절(다음 `## ` 앞까지)과 그 절의 bash 블록이다.
func pasteSection(t *testing.T, readme, head string, min int) (sec string, blocks []string) {
	t.Helper()
	_, sec, ok := strings.Cut(readmeBody(t, readme), head)
	if !ok {
		t.Fatalf("%s 에 %q 절이 없다", readme, strings.TrimSpace(head))
	}
	sec, _, _ = strings.Cut(sec, "\n## ")
	blocks = readmeBlocks(sec)
	if len(blocks) < min {
		t.Fatalf("%s 의 %q 절에서 bash 블록을 %d 개만 찾았다 - 절을 잘못 잘랐다", readme, strings.TrimSpace(head), len(blocks))
	}
	return sec, blocks
}

// TestREADMEPostgresBlocksHaveNoHashComments 는 README 의 붙여 넣는 절(「빠른 시작」 · 「Postgres 원장」)에 있는 bash 블록 어디에도
// 낱말 머리의 `#`(주석)가 없는지 잰다. 설명은 `: '…'` 나 산문이다.
//
// ★ 대화형 zsh(macOS 의 기본 셸)는 `interactivecomments` 가 꺼져 있어 `#` 를 주석으로 읽지 않는다: `#` 줄은 「command not found: #」
//
//	이고 `set -e` 덩이는 그 첫 줄에서 선다. 줄 끝의 `# …` 는 앞 명령의 인자가 되고(`f='…' # 자리를 채운다` 는 f 를 셸에 안 남긴다,
//	빠른 시작의 `curl … # {…}` 는 `#` 와 그 뒤를 curl 의 주소로 넘긴다). 절차 덩이는 TestREADMEProceduresStopWhole 이 `zsh -f -i`
//	로 돌려 잡지만, 준비 · 덤프 · 명령 설명 블록은 돌리지 않으므로 글자로 잰다.
func TestREADMEPostgresBlocksHaveNoHashComments(t *testing.T) {
	for _, tc := range pasteSections {
		_, blocks := pasteSection(t, tc.readme, tc.head, tc.min)
		for _, b := range blocks {
			bad, open := hashWords(b)
			for _, ln := range bad {
				t.Errorf("%s 의 %s 절 bash 블록에 `#` 주석이 있다 - 대화형 zsh 가 명령 · 인자로 돈다(`: '…'` 나 산문으로 옮긴다):\n  %s", tc.readme, strings.TrimSpace(tc.head), ln)
			}
			if open {
				t.Errorf("%s 의 bash 블록이 따옴표를 닫지 않는다(재는 쪽이 길을 잃었다):\n%s", tc.readme, b)
			}
		}
	}
}

// maskedQuery 는 docker 조회의 실패를 빈 출력으로 삼키는 꼴이다: 조건 안의 명령 치환(`[ -n "$(docker …)" ]`), 공백으로 가르는 위치
// 인자(`set -- $(docker …)`), 명령 치환을 바로 쓰는 cd · 변수(`cd "$(docker …)"`, `NAME="$(docker …)" cmd`), 종료 코드로 있음을
// 재는 inspect(`if docker inspect …`: 없는 것과 docker 가 선 것을 가르지 못한다).
var maskedQuery = regexp.MustCompile(`\[ -[nz] "\$\((docker|fdc) |set -- \$\((docker|fdc) |cd "\$\(docker |[A-Z_]+="\$\(docker |if !? ?docker inspect `)

// fencedLines 는 글의 코드 울타리(```) 안 줄들이다 - 산문(조회 꼴을 설명하는 인라인 코드)은 뺀다.
func fencedLines(body string) []string {
	var out []string
	in := false
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			in = !in
			continue
		}
		if in {
			out = append(out, ln)
		}
	}
	return out
}

// TestProceduresTakeDockerQueriesAsAssignments 는 README 의 붙여 넣는 절과 fd-update 스킬의 명령 블록이 docker 조회를 따로 선
// 대입(`x=$(docker …)`)으로만 받는지 잰다.
//
// ★ 대입은 docker 의 실패로 끝나 `set -e` 덩이 · `&&` 사슬이 그 자리에서 선다. 조건 안의 명령 치환은 docker 가 실패해도 빈 출력을
//
//	「없다 · 섰다」로 읽고 지나간다 - 72시간 되돌리기의 4 는 docker ps 가 실패하면 서버가 멈췄다고 보고 낸 파일을 맞대 놓았고,
//	갱신 사슬은 라벨을 못 읽으면 지금 자리 · 디렉토리 이름의 프로젝트를 내렸다. 도는 꼴은 TestREADMEProceduresStopWhole ·
//	TestUpdateSkillChainRunsInEveryShell 이 대역 docker 의 실패로 재고, 이 시험은 새로 쓰는 덩이가 그 꼴로 돌아가지 않게 글자로 잰다.
func TestProceduresTakeDockerQueriesAsAssignments(t *testing.T) {
	check := func(where string, lines []string) {
		for _, ln := range lines {
			if m := maskedQuery.FindString(ln); m != "" {
				t.Errorf("%s 의 명령이 docker 조회를 대입 아닌 꼴(%q)로 받는다 - 실패를 빈 출력으로 삼킨다(`x=$(docker …)` 로 떼어 받는다):\n  %s", where, m, strings.TrimSpace(ln))
			}
		}
	}
	for _, tc := range pasteSections {
		_, blocks := pasteSection(t, tc.readme, tc.head, tc.min)
		check(tc.readme+" "+strings.TrimSpace(tc.head), strings.Split(strings.Join(blocks, "\n"), "\n"))
	}
	for _, f := range []string{"SKILL.ko.md", "SKILL.md"} {
		raw, err := os.ReadFile(filepath.Join(pluginRoot(t), "skills", "fd-update", f))
		if err != nil {
			t.Fatal(err)
		}
		lines := fencedLines(string(raw))
		if len(lines) < 5 {
			t.Fatalf("fd-update/%s 에서 명령 블록 줄을 %d 개만 찾았다", f, len(lines))
		}
		check("fd-update/"+f, lines)
	}
}

// composeUp 은 compose 로 띄우는 자리다(fdc · docker compose 의 up -d, 사이의 -p · 프로젝트 인자 포함).
var composeUp = regexp.MustCompile(`(fdc|docker compose)( -p [^ ]+)? up -d( --build)?`)

// TestEveryUpBuilds 는 README 의 붙여 넣는 절(블록과 인라인 코드 모두)과 docs/server 의 서버 띄우기가 compose 의 `up -d` 를 모두
// `--build` 로 부르는지 잰다.
//
// ★ 이미지 이름이 판을 안 타는 고정 이름(`flightdeck:local` · `flightdeck-pg:local`)이다. `--build` 없는 up 은 그 이름의 이미지가
//
//	있으면 짓지 않는다 - 다른 판의 자리에서 지은 이미지로 뜬다. 켜기 전에 섰을 때 옛 판으로 다시 띄우는 up 은 업그레이드 3 이 새 판으로
//	지은 이미지를 집었고, 빠른 시작은 받은 판이 아니라 앞서 지은 판으로 떴다.
func TestEveryUpBuilds(t *testing.T) {
	texts := map[string]string{}
	for _, tc := range pasteSections {
		sec, _ := pasteSection(t, tc.readme, tc.head, tc.min)
		texts[tc.readme+" "+strings.TrimSpace(tc.head)] = sec
	}
	for _, f := range []string{"docs/server.ko.md", "docs/server.md"} {
		texts[f] = readmeBody(t, f)
	}
	total := 0
	for where, text := range texts {
		for _, m := range composeUp.FindAllStringSubmatch(text, -1) {
			total++
			if m[3] == "" {
				t.Errorf("%s 의 `%s` 가 --build 없이 띄운다 - 고정 이름의 옛 이미지로 뜬다", where, m[0])
			}
		}
	}
	if total < 20 {
		t.Fatalf("compose 의 up -d 를 %d 곳만 찾았다 - 재는 쪽이 자리를 잃었다", total)
	}
}

// hashWords 는 셸 글(block)에서 따옴표 밖 낱말 머리에 `#` 가 있는 줄과, 끝에서 따옴표 · 명령 치환이 열린 채인지를 낸다. 큰따옴표
// 안의 `$(…)` 는 따옴표 밖으로 다시 센다(그 안의 작은따옴표 · 큰따옴표가 바깥을 닫지 않는다). here-doc 의 몸은 건너뛴다.
func hashWords(block string) (bad []string, open bool) {
	heredoc := regexp.MustCompile(`<<-?\s*'?"?([A-Za-z_]+)'?"?`)
	type frame struct {
		kind  byte // 'u' 따옴표 밖 · 'd' 큰따옴표 · 's' 작은따옴표
		depth int  // 'u' 안의 맨 괄호 깊이 - 0 에서 만난 `)` 는 이 틀을 연 `$(` 를 닫는다
	}
	stack := []frame{{kind: 'u'}}
	until := ""
	for _, ln := range strings.Split(block, "\n") {
		if until != "" {
			if strings.TrimSpace(ln) == until {
				until = ""
			}
			continue
		}
		rs := []rune(ln)
		prev, esc, hit := ' ', false, false
		for k := 0; k < len(rs); k++ {
			c, top := rs[k], &stack[len(stack)-1]
			subst := c == '$' && k+1 < len(rs) && rs[k+1] == '('
			switch {
			case top.kind == 's':
				if c == '\'' {
					stack = stack[:len(stack)-1]
				}
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case subst:
				stack = append(stack, frame{kind: 'u'})
				k++
			case top.kind == 'd':
				if c == '"' {
					stack = stack[:len(stack)-1]
				}
			case c == '\'':
				stack = append(stack, frame{kind: 's'})
			case c == '"':
				stack = append(stack, frame{kind: 'd'})
			case c == '(':
				top.depth++
			case c == ')':
				if top.depth > 0 {
					top.depth--
				} else if len(stack) > 1 {
					stack = stack[:len(stack)-1]
				}
			case c == '#' && strings.ContainsRune(" \t;&|()", prev):
				hit = true
			}
			prev = c
		}
		if hit {
			bad = append(bad, ln)
		}
		if m := heredoc.FindStringSubmatch(ln); m != nil && stack[len(stack)-1].kind == 'u' {
			until = m[1]
		}
	}
	return bad, len(stack) != 1 || until != ""
}

// skillShells 는 fd-update 의 갱신 사슬을 돌릴 셸이다 - 붙여 넣는 대화형 셸에 더해, 사슬을 명령으로 돌리는 비대화형 셸(bash · zsh).
func skillShells(t *testing.T) []procShell {
	out := pasteShells(t)
	for _, s := range append([]procShell(nil), out...) {
		args := []string{"--norc", "--noprofile"}
		if strings.HasPrefix(s.name, "zsh") {
			args = []string{"-f"}
		}
		out = append(out, procShell{strings.Fields(s.name)[0] + " -c", s.path, args, false})
	}
	return out
}

// updateChain 은 fd-update 스킬(f)에서 내리기 · 새 자리 cd · 올리기 사슬을 떼어, 자리 표시(마켓플레이스 · 새 버전)를 M · V 로 채운다.
func updateChain(t *testing.T, f string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pluginRoot(t), "skills", "fd-update", f))
	if err != nil {
		t.Fatal(err)
	}
	var hit []string
	for _, c := range shellChains(string(raw)) {
		if strings.Contains(c, composeCmd+" down") && strings.Contains(c, composeCmd+" up -d") {
			hit = append(hit, c)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("fd-update/%s 에서 내리기 · 올리기 사슬이 %d 개다 - 하나여야 한다", f, len(hit))
	}
	return strings.NewReplacer("<마켓플레이스>", "M", "<새 버전>", "V", "<marketplace>", "M", "<new version>", "V").Replace(hit[0])
}

// TestUpdateSkillChainRunsInEveryShell 은 fd-update 의 갱신 사슬(마운트 읽기 → 관문 → 옛 자리에서 내리기 → 새 자리 cd → 올리기)을
// 스킬의 글자 그대로 bash(macOS 면 3.2) · zsh 에서, 대화형 · 비대화형으로 돌린다. 올리기가 새 자리에서 읽은 저장소 마운트 전부로
// 돌고, 마운트를 못 읽으면 아무 compose 도 안 부르는지 잰다.
//
// ★ 사슬의 첫 줄이 `readarray` 였을 때 zsh · bash 3.2 에서 그 줄이 127 로 서서 갱신이 아예 안 됐고(zsh 의 `${R[0]}` 는 비기도
//
//	한다), 시험은 사슬을 글자로만 봐 초록이었다. 그래서 실제로 돌린다.
func TestUpdateSkillChainRunsInEveryShell(t *testing.T) {
	for _, f := range []string{"SKILL.ko.md", "SKILL.md"} {
		chain := updateChain(t, f)
		for _, shell := range skillShells(t) {
			t.Run(f+"/"+shell.name, func(t *testing.T) {
				t.Parallel()
				r := newProcRig(t, shell, readmeProcs{})
				dst := filepath.Join(r.home, ".claude", "plugins", "cache", "M", "flightdeck", "V")
				if err := os.MkdirAll(dst, 0o700); err != nil {
					t.Fatal(err)
				}
				script := r.sub(chain) + "\necho \"BLOCK_RC=$?\""
				r.serverUp("oldp", r.old)
				rc, calls, out := r.exec(r.work, script)
				if rc != "0" {
					t.Fatalf("갱신 사슬이 %s 로 섰다:\n%s", rc, out)
				}
				if c := called(calls, "down"); !ran(c, r.old, "") {
					t.Errorf("내리기가 도는 서버의 자리에서 안 돌았다: %q", c)
				}
				if c := called(calls, "up -d --build"); !ran(c, dst, "") || !strings.Contains(c, " REPOS=/repos/one,/repos/two,, ") {
					t.Errorf("올리기가 새 자리 · 읽은 저장소 마운트로 안 돌았다: %q\n%s", c, out)
				}
				// 서버가 없으면(마운트를 못 읽는다) 아무것도 내리지도 올리지도 않는다.
				_ = os.Remove(filepath.Join(r.state, "created"))
				_ = os.Remove(filepath.Join(r.state, "up"))
				rc, calls, out = r.exec(r.work, script)
				if rc == "0" || len(composeCalls(calls)) != 0 {
					t.Errorf("마운트를 못 읽었는데 사슬이 compose 를 불렀다(rc=%s):\n%s\n%s", rc, strings.Join(calls, "\n"), out)
				}
				// 공백이 든 저장소 경로는 한 경로다 - docker 가 낸 한 줄이 슬롯 하나로 간다.
				r.serverUp("oldp", r.old)
				r.env["FDT_MOUNTS"] = "/repos/team one\n/repos/two"
				rc, calls, out = r.exec(r.work, script)
				if c := called(calls, "up -d --build"); rc != "0" || !strings.Contains(c, " REPOS=/repos/team one,/repos/two,, ") {
					t.Errorf("공백 경로가 올리기에 한 경로로 안 넘어갔다(rc=%s): %q\n%s", rc, c, out)
				}
				// ★ 내리기 **전에** 서는 자리들: 조회(라벨 · 마운트 · 서버 env · 덤프)가 실패하거나, 마운트가 없거나 넷을 넘는다. 어느
				//   것이든 docker 의 빈 출력을 「없다 · 섰다」로 읽고 지나가면 다른 자리 · 다른 프로젝트를 내리거나 마운트를 잃는다.
				for _, tc := range []struct{ name, mounts, fail string }{
					{"라벨 조회가 실패한다", "", "Labels"},
					{"마운트 조회가 실패한다", "", "Mounts"},
					{"서버 env 조회가 실패한다", "", "Config.Env"},
					{"덤프 조회가 실패한다", "", "^ps -q --filter label=com.docker.compose.service=db-backup"},
					{"마운트가 없다", "-", ""},
					{"마운트가 넷을 넘는다", "/r/1\n/r/2\n/r/3\n/r/4\n/r/5", ""},
				} {
					r.serverUp("oldp", r.old)
					delete(r.env, "FDT_MOUNTS")
					r.env["FDT_FAIL"] = tc.fail
					switch tc.mounts {
					case "":
					case "-":
						r.env["FDT_MOUNTS"] = ""
					default:
						r.env["FDT_MOUNTS"] = tc.mounts
					}
					rc, calls, out = r.exec(r.work, script)
					if rc == "0" || len(composeCalls(calls)) != 0 {
						t.Errorf("%s: 사슬이 서지 않고 compose 를 불렀다(rc=%s):\n%s\n%s", tc.name, rc, strings.Join(calls, "\n"), out)
					}
				}
			})
		}
	}
}
