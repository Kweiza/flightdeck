package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// 셋업 판정 — **관측과 판정을 가른다.**
//
// 관측(OS·go 버전·docker 유무·설정 출처·healthz)은 부수효과 있는 자리에서 하고,
// 여기 있는 것은 그 값만 보는 순수 함수다. 그래야 조합 전체를 시험이 표로 돌 수 있다.
// 이 레포의 다른 판정기(JudgeOffline·PlanMigration·GateTool)가 전부 같은 모양이다.

// MinGoVersion 은 이 소스를 빌드할 수 있는 최소 Go 다. go.mod 의 값과 같아야 한다.
//
// ★ 상수로 두는 이유가 실측에 있다: Ubuntu apt 의 golang-go 후보가 **1.22** 라
// (2026-08-04 `apt-cache policy golang-go` → `2:1.22~2build1`) 존재 검사만 하면
// 설치는 성공하고 빌드가 깨진다. 그 함정을 이 상수와 GoVersionOK 가 막는다.
const MinGoVersion = "1.25"

// Role 은 이 머신이 서버인가 클라이언트인가다.
type Role string

const (
	RoleServer  Role = "server"
	RoleClient  Role = "client"
	RoleUnknown Role = "unknown"
)

// DetectRole 은 주소만 보고 역할을 정한다. 순수 함수다.
//
// ★ **역할을 저장하지 않는다.** 루프백 주소는 "이 머신이 서버다" 말고 다른 뜻이 될 수 없으므로
// 파생이 성립한다. 파일에 저장하면 의도와 현실이 갈린다 — 저장된 "서버"인데 주소는 원격인
// 상태를 아무도 못 막는다. 파생 가능한 값에 파라미터를 두지 않는 것이 이 레포의 규율이다.
func DetectRole(raw string) Role {
	s := strings.TrimSpace(raw)
	if s == "" {
		return RoleUnknown
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return RoleUnknown
	}
	host := u.Hostname()
	if host == "" {
		return RoleUnknown
	}
	if strings.EqualFold(host, "localhost") {
		return RoleServer
	}
	if ip := net.ParseIP(host); ip != nil {
		// 0.0.0.0 은 "전 인터페이스로 듣는다"라 클라이언트 목적지로는 성립하지 않는다 —
		// 이 머신이 서버라는 뜻으로 읽는 것이 유일하게 말이 된다.
		if ip.IsLoopback() || ip.IsUnspecified() {
			return RoleServer
		}
		return RoleClient
	}
	return RoleClient
}

var goVersionRe = regexp.MustCompile(`go(\d+)\.(\d+)(?:\.(\d+))?`)

// GoVersionOK 는 `go version` 출력이 이 소스를 빌드할 수 있는지 본다. 순수 함수다.
//
// 사유를 **항상** 낸다. "안 된다"만 알면 무엇을 설치해야 하는지 모른다.
func GoVersionOK(goVersionOutput string) (ok bool, why string) {
	s := strings.TrimSpace(goVersionOutput)
	if s == "" {
		return false, "Go 툴체인이 없다 — 런처가 fd 를 빌드하지 못한다(필요: " + MinGoVersion + " 이상)"
	}
	m := goVersionRe.FindStringSubmatch(s)
	if m == nil {
		return false, fmt.Sprintf("Go 버전을 못 읽었다(%q) — 필요: %s 이상", clip(s, 120), MinGoVersion)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	wantMajor, wantMinor := 1, 25
	got := fmt.Sprintf("%d.%d", major, minor)
	if m[3] != "" {
		got += "." + m[3]
	}
	if major > wantMajor || (major == wantMajor && minor >= wantMinor) {
		return true, "Go " + got + " — 빌드할 수 있다(필요: " + MinGoVersion + " 이상)"
	}
	return false, fmt.Sprintf("Go %s 가 설치돼 있는데 이 소스는 %s 이상이 필요하다 — "+
		"이대로면 빌드가 깨진다", got, MinGoVersion)
}

// SetupState 는 **관측된 값 전부**다. 여기 있는 것은 부수효과 있는 자리에서 잰 것이고,
// 판정은 이 값만 본다.
type SetupState struct {
	OS        string // runtime.GOOS
	Distro    string // linux 일 때만. "ubuntu"·"debian"·"fedora"·"arch"·"" (모름)
	GoOutput  string // `go version` 출력 그대로. 없으면 빈 문자열
	Docker    bool   // docker 가 PATH 에 있나
	Endpoint  Endpoint
	Reachable bool // healthz 가 답했나

	// PluginRoot 는 compose.yaml 이 있는 플러그인 트리다(composeRoot). 비면 모른다.
	// 서버 기동 명령이 그 자리로 cd 한다 — /fd-setup 은 명령을 세션의 작업 디렉토리에서 돌린다.
	PluginRoot string
}

// SetupStep 은 사람에게 보여 주고 승인을 받을 명령 하나다.
//
// ★ 명령과 **왜 그것인지**를 함께 나른다. 승인을 구하는 화면에 이유가 없으면
// 사용자는 자기 머신에 무엇이 왜 들어가는지 모른 채 예를 누른다.
type SetupStep struct {
	What    string // 무엇을 하는가
	Command string // 실행할 명령 그대로
	Why     string // 왜 필요한가
	Sudo    bool   // 관리자 권한을 요구하는가
}

// SetupPlan 은 판정 결과다.
type SetupPlan struct {
	Role      Role
	Ready     bool // 지금 그대로 쓸 수 있나
	Refused   bool // 이 환경은 지원하지 않는다(Windows)
	Reason    string
	Steps     []SetupStep
	Verify    string // 다 하고 나서 무엇으로 확인하나
	Endpoint  Endpoint
	Reachable bool
}

// PlanSetup 은 관측값만 보고 할 일을 정한다. 순수 함수다.
func PlanSetup(s SetupState) SetupPlan {
	p := SetupPlan{
		Role: DetectRole(s.Endpoint.URL), Endpoint: s.Endpoint, Reachable: s.Reachable,
		Verify: "fd doctor  — 좌표·서버·아웃박스를 한 화면에 낸다",
	}

	// ★ Windows 는 **거절한다.** 셋업 성공을 흉내내면 훅·MCP 가 왜 안 뜨는지 영영 모른다.
	// 진입점 bin/fd 가 bash 스크립트인데, Claude Code 는 훅 shell-form 을 Git Bash 없는
	// Windows 에서 PowerShell 로 돌리고, .mcp.json 스키마에는 OS 분기 필드가 없다.
	// 즉 스킬이 아무리 잘 돌아도 플러그인 자체가 안 뜬다.
	if s.OS == "windows" {
		p.Refused = true
		p.Reason = "이 플러그인은 아직 Windows 에서 동작하지 않는다. 진입점(bin/fd)이 bash 스크립트라 " +
			"훅과 MCP 서버가 뜨지 않는다 — Claude Code 는 Git Bash 가 없는 Windows 에서 훅 명령을 " +
			"PowerShell 로 돌리고, .mcp.json 에는 OS 분기 수단이 없다.\n" +
			"쓰려면 **WSL(Ubuntu) 안에서 Claude Code 를 실행해라** — 그 안에서는 이 셋업이 그대로 돈다."
		p.Verify = ""
		return p
	}

	if ok, why := GoVersionOK(s.GoOutput); !ok {
		p.Steps = append(p.Steps, goInstallStep(s, why))
	}

	switch p.Role {
	case RoleServer:
		if !s.Reachable {
			p.Steps = append(p.Steps, serverStartSteps(s)...)
		}
	case RoleClient:
		if !s.Reachable {
			p.Reason = fmt.Sprintf("클라이언트로 설정돼 있는데 %s 에 못 닿았다(출처: %s). "+
				"주소가 맞는지, 저쪽 서버가 떠 있는지 확인해라 — **이 머신에 서버를 세우는 것은 답이 아니다.**",
				clip(s.Endpoint.URL, 200), clip(s.Endpoint.URLSource, 120))
		}
	case RoleUnknown:
		p.Reason = fmt.Sprintf("서버 주소를 못 읽었다(%q, 출처: %s) — "+
			"`fd setup --url <주소>` 로 정해라", clip(s.Endpoint.URL, 200), clip(s.Endpoint.URLSource, 120))
	}

	p.Ready = len(p.Steps) == 0 && s.Reachable && p.Role != RoleUnknown
	if p.Ready && p.Reason == "" {
		p.Reason = "이미 셋업돼 있다 — 바꿀 것이 없다"
	}
	return p
}

// goInstallStep 은 OS 별 Go 설치 명령이다.
//
// ★ **Ubuntu·Debian 에서 apt 를 쓰지 않는다.** apt 의 golang-go 후보는 1.22 라
// (2026-08-04 실측) 설치는 성공하고 빌드가 깨진다. snap 이 현행 Go 를 준다.
func goInstallStep(s SetupState, why string) SetupStep {
	switch {
	case s.OS == "darwin":
		return SetupStep{
			What: "Go 툴체인 설치", Command: "brew install go", Why: why,
		}
	case s.Distro == "ubuntu" || s.Distro == "debian":
		return SetupStep{
			What:    "Go 툴체인 설치",
			Command: "sudo snap install go --classic",
			Why:     why + " · apt 의 golang-go 는 1.22 라 못 쓴다(빌드가 깨진다)",
			Sudo:    true,
		}
	case s.Distro == "fedora" || s.Distro == "rhel":
		return SetupStep{
			What: "Go 툴체인 설치", Command: "sudo dnf install -y golang", Why: why, Sudo: true,
		}
	case s.Distro == "arch":
		return SetupStep{
			What: "Go 툴체인 설치", Command: "sudo pacman -S --noconfirm go", Why: why, Sudo: true,
		}
	default:
		return SetupStep{
			What:    "Go 툴체인 설치",
			Command: "https://go.dev/dl/ 에서 " + MinGoVersion + " 이상을 받아 설치해라",
			Why:     why + " · 이 배포판의 패키지 관리자를 모른다",
		}
	}
}

// composeEnvPath 는 이 기계의 배포가 고른 것(README 「Postgres 로 띄우기」의 선택 파일 compose.env)을 적는 자리다.
//
// ★ 서버의 어느 마운트에도 안 걸리는 자리다(/data = ~/.flightdeck · /ledger · FD_REPOS, 기본 홈 전체). 서버가 쓸 수 있는 자리에
// 두면 서버가 바꾼 설정(덮개 · 시크릿 자리)을 다음 갱신이 그대로 믿는다 - 갱신은 사람이, 때로 root 로 돌린다. 시험
// (TestDeploySettingsSitOutsideServerMounts)이 렌더한 마운트와 이 자리를 맞댄다.
const composeEnvPath = "/etc/flightdeck/compose.env"

// composeEnvSel 은 선택 파일이 켜져 있으면(composeEnvPath 가 있으면) 그 자리를, 꺼져 있으면 빈 값을 내는 셸 한 마디다. compose
// 머리(composeCmd)와 README 의 fdc(꺼 둔 파일을 FDC_ENV 로 대신 줄 수 있다)가 이 글자 하나로 고른다 - 시험이 둘을 맞댄다.
const composeEnvSel = `$([ ! -e ` + composeEnvPath + ` ] || echo ` + composeEnvPath + `)`

// composeCmd 는 이 기계의 배포가 고른 덮개 파일을 얹어 docker compose 를 부르는 셸 머리다. composeEnvPath 가 있으면 그것을
// compose 의 env 파일로 주고(그 안의 COMPOSE_FILE 이 Postgres 덮개를 얹는다), 없으면 빈 값이라 기본 compose(SQLite)다. 빈
// COMPOSE_ENV_FILES 는 안 준 것과 같다.
//
// ★ 서버를 띄우고 갱신하는 안내가 이 하나를 쓴다(fd setup 의 기동 단계 · fd-update 스킬 - 스킬은 시험이 이 글자와 맞댄다 ·
// README 의 fdc 는 composeEnvSel 을 쓴다). 안내마다 따로 쓰면 한 곳이 덮개를 빼고, 덮개를 뺀 `up` 은 Postgres 로 옮긴 배포를
// SQLite 로 띄우려 든다(그 길은 이관 표지가 막지만, 막힌 기동은 서버가 없는 것으로 보인다).
// ★ 이 머리를 부르는 명령 사슬은 composeEnvGuard 로 시작한다(아래).
const composeCmd = `COMPOSE_ENV_FILES="` + composeEnvSel + `" docker compose`

// composeEnvGuard 는 선택 파일(composeEnvPath)이 이 배포의 고른 것을 정하는 **한 곳**이 아니면 처방을 내고 거짓으로 끝나는 셸
// 한 마디다. compose 머리를 부르는 모든 안내(fd setup 의 기동 · fd-update 스킬의 모든 사슬 · README 의 fdc)가 그 앞에 문다 -
// 시험(TestComposeGuidanceSharesOneHead · TestDeploySettingsSitOutsideServerMounts)이 그 글자를 맞댄다. 두 갈래다:
//
//   - 선택 파일의 옛 자리(~/.flightdeck/compose.env)에 파일이 남아 있다. 옛 자리는 서버가 쓰는 /data 안이라 읽지 않는다
//     (composeEnvPath 의 ★). 다만 말없이 건너뛰면 그 파일로 고른 Postgres 배포가 기본 compose(SQLite)로 뜨려 들어(이관 표지가
//     막는다) 서버가 없는 것으로만 보인다 - 그래서 멈추고 옮기는 법을 낸다. 서버가 바꿨을 수 있으니 옮기기 전에 내용을 본다.
//   - 셸이 어느 원장인지를 정하는 값(FD_PG_INSTANCE - db 컨테이너 · 두 볼륨의 이름, FD_PG_DATABASE)을 준다. compose 는 셸 값을
//     선택 파일보다 먼저 읽는다 - 한 셸에서 준 값으로 옮기고, 그 값이 빠진 새 셸에서 이어 띄우면 다른 볼륨(빈 원장이나 다른
//     배포의 원장)을 고른다. 값은 선택 파일에만 둔다(README 준비 4 가 적는다). 같은 값이어도 거절한다 - 비교할 자리를 셸에
//     두면 그것이 둘째 정본이다. 빈 값도 준 것이다(`+x`): compose 는 셸의 빈 값도 선택 파일보다 먼저 읽는다.
//
// ★ if 한 덩이다. `a && [ … ] || { … } && b` 로 이으면 셸이 `((a && [ … ]) || { … }) && b` 로 묶어, a 가 실패해도 이 처방을 낸다.
// ★ 홑따옴표 없이 쓴다 - ~ 가 풀려야 한다. sh · bash · zsh 가 같게 푼다. 문구 안에는 홑따옴표를 쓰지 않는다.
const composeEnvGuard = `if [ -e ~/.flightdeck/compose.env ]; then echo '선택 파일의 옛 자리 ~/.flightdeck/compose.env 는 ` +
	`서버가 쓸 수 있는 /data 안이라 더는 읽지 않는다 - 내용을 확인한 뒤 ` + composeEnvPath + ` 로 옮기고 다시 돌린다: ` +
	`sudo install -d -m 0700 -o "$(id -u)" -g "$(id -g)" /etc/flightdeck && mv -n ~/.flightdeck/compose.env ` + composeEnvPath +
	` (README 「Postgres 로 띄우기」)' >&2; false; ` +
	`elif [ -n "${FD_PG_INSTANCE+x}${FD_PG_DATABASE+x}" ]; then echo 'FD_PG_INSTANCE · FD_PG_DATABASE 는 셸에서 주지 않는다 - ` +
	`어느 원장(볼륨 · DB)인지는 선택 파일 ` + composeEnvPath + `(꺼 두었으면 .off) 한 곳에 적는다. 셸 값은 선택 파일보다 먼저라 ` +
	`새 셸에서 빠지면 다른 원장으로 뜬다: 값을 선택 파일에 적고 unset FD_PG_INSTANCE FD_PG_DATABASE 뒤 다시 돌린다 ` +
	`(README 「Postgres 로 띄우기」)' >&2; false; fi`

// serverStartSteps 는 서버를 띄우는 길이다.
//
// ★ **둘을 구분해서 낸다.** 이 레포에는 상시 실행의 정식 방법이 없다(systemd·launchd 정의가
// 하나도 없다). docker compose 만 재시작 정책을 갖고 있으므로 그것을 지원 경로로,
// 포그라운드 실행은 "지금만"으로 이름 붙인다. 없는 것을 있는 척하지 않는다.
//
// ★ compose 명령은 **compose.yaml 이 있는 자리에서** 돌아야 한다. /fd-setup 은 이 명령을
// 세션의 작업 디렉토리에서 그대로 돌리므로, 자리를 알면 cd 를 명령에 박는다. 모르면
// 지어내지 않고 어디서 돌려야 하는지를 What 에 말로 남긴다.
// FD_UID·FD_GID 는 compose 가 기본 1000 으로 채우는 값이다 — 호스트 사용자가 아니면 DB 를
// 열되 못 쓰고 git 이 소유권을 의심한다(compose.yaml 주석).
// ★ `--build` 로 띄운다(fd-update 의 올리기 · README 의 띄우기와 같다). 이미지 이름이 판을 안 타는 고정 이름이라
// (flightdeck:local · flightdeck-pg:local) 앞 판이 지은 이미지가 남아 있으면 `up -d` 는 그것으로 뜬다 - 이 판의 접속 규칙 ·
// 계정 스크립트 대신 옛것으로. 짓기는 캐시가 맞으면 아무것도 안 한다.
func serverStartSteps(s SetupState) []SetupStep {
	if s.Docker {
		const up = composeEnvGuard + " && FD_UID=$(id -u) FD_GID=$(id -g) " + composeCmd + " up -d --build"
		if s.PluginRoot != "" {
			return []SetupStep{{
				What:    "서버 기동(지원되는 상시 실행 경로)",
				Command: "cd " + setupShellQuote(s.PluginRoot) + " && " + up,
				Why:     "restart 정책이 있어 재부팅·크래시를 넘긴다. DB 는 ~/.flightdeck 에 붙는다",
			}}
		}
		return []SetupStep{{
			What:    "서버 기동(지원되는 상시 실행 경로 — flightdeck 루트, compose.yaml 이 있는 자리에서)",
			Command: up,
			Why:     "restart 정책이 있어 재부팅·크래시를 넘긴다. DB 는 ~/.flightdeck 에 붙는다",
		}}
	}
	return []SetupStep{{
		What:    "서버 기동(임시 — 이 터미널을 닫으면 죽는다)",
		Command: "fd serve --addr 127.0.0.1:7420",
		Why: "docker 가 없다. 이 레포에는 systemd·launchd 정의가 없으므로 " +
			"상시 실행을 원하면 docker 를 설치하거나 서비스 정의를 직접 만들어야 한다",
	}}
}

// RenderSetupPlan 은 계획을 사람이 읽는 문구로 만든다. 순수 함수다.
//
// 이 문자열이 소비자 좌표계다 — 스킬이 읽는 것도, 사람이 읽는 것도 이것뿐이다.
func RenderSetupPlan(p SetupPlan) string {
	var b strings.Builder

	if p.Refused {
		b.WriteString("■ 이 환경은 지원하지 않는다\n")
		b.WriteString("  " + strings.ReplaceAll(p.Reason, "\n", "\n  ") + "\n")
		return b.String()
	}

	b.WriteString("■ 지금 상태\n")
	fmt.Fprintf(&b, "  역할      %s\n", roleLabel(p.Role))
	fmt.Fprintf(&b, "  서버      %s (%s)\n", orDash(p.Endpoint.URL), clip(p.Endpoint.URLSource, 120))
	fmt.Fprintf(&b, "  토큰      %s\n", clip(p.Endpoint.TokenSource, 120))
	// 역할과 도달성은 **따로** 낸다 — 설정 문제와 기동 문제는 다른 사실이다.
	if p.Reachable {
		b.WriteString("  도달      ✓ healthz 가 답한다\n")
	} else {
		b.WriteString("  도달      ✗ 응답이 없다\n")
	}
	if strings.TrimSpace(p.Endpoint.Warn) != "" {
		fmt.Fprintf(&b, "  ! %s\n", clip(p.Endpoint.Warn, 400))
	}
	if strings.TrimSpace(p.Reason) != "" {
		fmt.Fprintf(&b, "  %s\n", strings.ReplaceAll(p.Reason, "\n", "\n  "))
	}

	if len(p.Steps) == 0 {
		if p.Ready {
			b.WriteString("\n■ 할 일 없음\n")
		}
	} else {
		b.WriteString("\n■ 할 일 — **실행 전에 사람의 승인을 받아라**\n")
		for i, s := range p.Steps {
			mark := ""
			if s.Sudo {
				mark = " [관리자 권한]"
			}
			fmt.Fprintf(&b, "  %d. %s%s\n     $ %s\n     왜: %s\n", i+1, s.What, mark, s.Command, s.Why)
		}
	}
	if strings.TrimSpace(p.Verify) != "" {
		fmt.Fprintf(&b, "\n■ 확인\n  %s\n", p.Verify)
	}
	return b.String()
}

func roleLabel(r Role) string {
	switch r {
	case RoleServer:
		return "서버 — 이 머신이 조정 서버를 띄운다(주소가 루프백이다)"
	case RoleClient:
		return "클라이언트 — 다른 머신의 서버에 붙는다"
	default:
		return "모름 — 주소를 못 읽었다"
	}
}

// reportCredentialSwitchFlush 는 토큰을 바꾸기 직전 재생의 결과를 낸다. 빈 큐면 아무 말도 안 한다.
//
// ★ 남은 줄이 있어도 저장은 막지 않는다. 옛 토큰이 철회돼 401 을 받는 큐는 옛 자격증명으로 영영 못 비우는데,
// 그런 줄은 서버가 기록하지 않았으니 새 토큰으로 보내도 두 번 남지 않는다. 막으면 그 사람이 새 토큰으로 가는
// 길만 끊긴다. 대신 두 번 남을 수 있는 경우(닿았는데 답을 못 받은 줄)를 말하고 판단은 사람에게 둔다.
func reportCredentialSwitchFlush(out io.Writer, res ReplayResult) {
	switch {
	case res.Remaining > 0:
		fmt.Fprintf(out, "★ 아웃박스에 %d건이 남은 채 토큰을 바꾼다. 옛 토큰으로 서버에 닿았는데 답을 못 받은 줄이면 "+
			"새 토큰의 재생이 그 판단을 한 번 더 남긴다(멱등 키는 주체마다 따로다). 옛 토큰으로 서버에 닿을 수 있으면 "+
			"먼저 비우고 바꿔라: %s\n", res.Remaining, clip(res.Detail, 300))
	case res.Sent > 0 || res.Rejected > 0:
		fmt.Fprintf(out, "토큰을 바꾸기 전에 아웃박스를 옛 자격증명으로 비웠다: %s\n", clip(res.Detail, 300))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// `fd setup` — 관측하고 판정을 낸다. **아무것도 설치하지 않고 아무것도 띄우지 않는다.**
//
// ★ 왜 이 명령이 실행을 안 하나: 설치는 sudo/관리자를 요구하고 되돌리기 어렵다.
// 그래서 이 자리는 "무엇이 없고 다음에 무엇을 해야 하나"까지만 내고, **승인을 받아 실행하는
// 일은 사람(또는 fd-setup 스킬)이** 한다. 도구가 조용히 시스템을 바꾸지 않는다.
// ─────────────────────────────────────────────────────────────────────────────

// runSetup 은 `fd setup` 이다.
func (a *App) runSetup(ctx context.Context, args []string, out io.Writer) int {
	fs := newFlagSet("setup")
	setURL := fs.String("url", "", "서버 주소를 정한다(예: https://fd.example.internal:7420). 이 머신이 서버면 "+DefaultURL+
		". 원격은 https 다 - 토큰은 평문 http 로 루프백 밖에 안 나간다("+InsecureHTTPEnv+"=1 이 탈출구)")
	setToken := fs.String("token", "", "서버 토큰. --url 과 함께 쓴다")
	clearToken := fs.Bool("clear-token", false, "저장된 토큰을 지운다")
	installCodex := fs.Bool("install-codex", false,
		"codex 훅 자산을 깐다(고정 경로 래퍼 + ~/.codex/hooks.json). 신뢰는 사람이 TUI 에서 누른다")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// ── 설치 갈래 ───────────────────────────────────────────────────────────
	//
	// ★ 이것이 이 명령에서 **유일하게 무언가를 쓰는** 갈래이고, 그래서 **명시적 플래그를
	// 요구한다.** 머리말의 판정("도구가 조용히 시스템을 바꾸지 않는다")은 그대로다 —
	// 보고 갈래는 이 명령을 화면에 낼 뿐이고, 사람이 그것을 승인해서 부를 때만 깔린다.
	// 그리고 여기서도 **신뢰는 안 박는다**: 그것은 사용자가 TUI 에서 누르는 관문이다.
	if *installCodex {
		return a.InstallCodex(out)
	}

	home := homeDir(a.env)
	path, pathSrc := ConfigPath(a.env, home)

	// ── 저장 갈래 ───────────────────────────────────────────────────────────
	if strings.TrimSpace(*setURL) != "" || strings.TrimSpace(*setToken) != "" || *clearToken {
		cfg, _, _ := LoadConfig(path)
		oldToken := cfg.Token
		if v := strings.TrimSpace(*setURL); v != "" {
			cfg.URL = strings.TrimRight(v, "/")
		}
		if *clearToken {
			cfg.Token = ""
		} else if v := strings.TrimSpace(*setToken); v != "" {
			cfg.Token = v
		}
		// ★ 토큰을 바꾸기 전에 아웃박스를 **지금 자격증명으로** 비운다. 멱등 표의 키는 주체 범위가 붙어서
		//   (`ScopeIdempotencyKey`), 옛 토큰으로 서버에 닿았는데 답을 못 받은 줄을 새 토큰이 다시 보내면 서버는
		//   다른 키로 읽고 판단을 한 번 더 남긴다. 서버가 원문 키로 되돌아가 찾으면 주체 경계가 무너지므로 그 틈은
		//   자격증명을 바꾸는 이 자리에서 닫는다. a.cli 는 기동 때 읽은 자격증명을 들고 있어 저장 전후와 무관하게 옛 것이다.
		if cfg.Token != oldToken {
			reportCredentialSwitchFlush(out, a.cli.Flush(ctx))
		}
		if err := SaveConfig(path, cfg); err != nil {
			fmt.Fprintf(out, "설정을 저장하지 못했다: %v\n", err)
			return 1
		}
		fmt.Fprintf(out, "설정 저장 · %s (%s)\n", path, pathSrc)
		fmt.Fprintf(out, "  서버 %s · 역할 %s\n", orDash(cfg.URL), roleLabel(DetectRole(cfg.URL)))
		fmt.Fprintf(out, "  토큰 %s\n", map[bool]string{true: "설정됨", false: "없음"}[cfg.Token != ""])
		// ★ 이 줄을 반드시 낸다. MCP 서버는 **기동 시 환경을 한 번 읽고 끝**이고
		//   훅도 부모(claude)의 환경을 물려받는다. 안 말하면 사용자는
		//   "설정했는데 안 된다"를 겪고 원인을 못 찾는다.
		fmt.Fprintln(out, "\n★ 지금 도는 세션에는 안 붙는다 — Claude Code 를 다시 시작해야 훅·MCP 가 이 값을 본다.")
		return 0
	}

	// ── 보고 갈래 ───────────────────────────────────────────────────────────
	st := a.observeSetup(ctx)
	fmt.Fprint(out, RenderSetupPlan(PlanSetup(st)))
	// ★ codex 절은 **PlanSetup 밖**이다. 그 함수는 "이 머신이 fd 를 돌릴 수 있나"를 판정하고,
	// 이쪽은 "이 머신의 두 번째 하네스에 설치물이 붙었나"라 축이 다르다. 한 판정기에 넣으면
	// Ready 의 뜻이 갈린다 — codex 가 안 깔린 것은 fd 가 못 도는 것이 아니다.
	if st.OS != "windows" {
		fmt.Fprint(out, RenderCodexSetup(a.observeCodex()))
		fmt.Fprintf(out, "\n설정 파일  %s (%s)\n", path, pathSrc)
	}
	return 0
}

// composeRoot 는 런처가 넘긴 플러그인 트리(FD_PLUGIN_ROOT)에 compose.yaml 이 **실제로 있을 때만**
// 그 자리를 낸다. 없으면 빈 문자열이다 — 있는 척 cd 를 박으면 명령이 엉뚱한 자리에서 실패한다.
func composeRoot(get func(string) (string, bool)) string {
	v, ok := get("FD_PLUGIN_ROOT")
	root := strings.TrimSpace(v)
	if !ok || root == "" {
		return ""
	}
	if fi, err := os.Stat(filepath.Join(root, "compose.yaml")); err != nil || fi.IsDir() {
		return ""
	}
	return root
}

// setupShellQuote 는 POSIX 셸의 작은따옴표 인용이다(service/pick.go 의 shellQuote 와 같은 규칙).
func setupShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// observeSetup 은 판정에 필요한 값을 **실제로 잰다.** 판정은 안 한다(PlanSetup 이 한다).
func (a *App) observeSetup(ctx context.Context) SetupState {
	st := SetupState{
		OS:       runtime.GOOS,
		Distro:   detectDistro(),
		GoOutput: commandOutput(ctx, "go", "version"),
		Docker:   lookPath("docker"),
		Endpoint: a.cli.Endpoint,

		PluginRoot: composeRoot(a.env),
	}
	// 도달성은 healthz 로 잰다 — 인증 게이트 앞이라 토큰 없이도 답한다.
	if _, err := a.cli.Healthz(ctx); err == nil {
		st.Reachable = true
	}
	return st
}

// detectDistro 는 /etc/os-release 의 ID 를 읽는다. 못 읽으면 빈 문자열이다.
//
// 지어내지 않는다 — 모르면 모른다고 두고, 그때 계획은 공식 다운로드 페이지를 낸다.
func detectDistro() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ID="); ok {
			return strings.ToLower(strings.Trim(strings.TrimSpace(v), `"`))
		}
	}
	return ""
}

func lookPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// commandOutput 은 명령 하나를 돌려 stdout 을 낸다. 실패하면 빈 문자열이다.
func commandOutput(ctx context.Context, bin string, args ...string) string {
	if !lookPath(bin) {
		return ""
	}
	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
