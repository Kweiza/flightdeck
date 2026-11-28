package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kweiza/flightdeck/internal/api"
)

// fd admin - 계정 단계의 관리자 손잡이 둘.
//
//   - bootstrap: 관리자가 0명인 서버에 FD_TOKEN 으로 첫 관리자를 만든다(POST /admin/bootstrap). 서버를 거친다.
//   - recover: 관리자 계정을 못 쓰게 됐을 때 **서버 컨테이너 안에서** 원장을 직접 열어 새 로그인 코드를 낸다. HTTP 를 안 탄다 -
//     accounts 단계에서 FD_TOKEN 은 관리자가 생긴 뒤 HTTP 어디서도 안 통하므로(auth.Gate), 원장에 닿는 사람만 복구한다.
//
// ★ 로그인 코드는 stdout 에 **한 번** 찍는다. 로그(stderr)에는 싣지 않는다 - 서버도 원값을 안 남긴다(sha256 만).

const adminUsage = `fd admin bootstrap --login <이름> [--name <표시 이름>] [--email <메일>]
                                          관리자가 0명인 서버에 FD_TOKEN 으로 첫 관리자를 만들고 로그인 코드를 낸다
fd admin recover <login> [--db <원장>]   서버 컨테이너 안에서 원장을 직접 열어 그 관리자의 새 로그인 코드를 낸다
                                          (꺼진 관리자면 다시 켠다)
`

func (a *App) runAdmin(ctx context.Context, args []string, out io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(out, adminUsage)
		return 2
	}
	switch args[0] {
	case "bootstrap":
		return a.runAdminBootstrap(ctx, args[1:], out)
	case "recover":
		return a.runAdminRecover(ctx, args[1:], out)
	default:
		fmt.Fprintf(out, "모르는 admin 명령: %s\n\n%s", clip(args[0], 40), adminUsage)
		return 2
	}
}

func (a *App) runAdminBootstrap(ctx context.Context, args []string, out io.Writer) int {
	fs := newFlagSet("admin bootstrap")
	login := fs.String("login", "", "첫 관리자의 로그인 이름(소문자 · 숫자 · . _ -)")
	name := fs.String("name", "", "표시 이름(비면 로그인 이름)")
	email := fs.String("email", "", "메일(선택)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*login) == "" {
		fmt.Fprint(out, "--login 이 없다\n\n"+adminUsage)
		return 2
	}
	if a.cli.Token == "" {
		fmt.Fprintln(out, "FD_TOKEN 이 비어 있다 - 부트스트랩은 서버의 FD_TOKEN 으로만 부른다")
		return 2
	}
	body := map[string]string{"login": *login, "display_name": *name, "email": *email}
	// ★ 멱등 키를 안 싣는다. 서버가 이 경로를 멱등 표에 안 넣는다(api.JudgeSecretResponse) - 응답이 비밀이라서다.
	raw, _, err := a.cli.do(ctx, http.MethodPost, api.BootstrapPath, body, "")
	if err != nil {
		fmt.Fprintf(out, "부트스트랩 실패: %s\n", err)
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusConflict {
			fmt.Fprintln(out, "관리자가 이미 있다면 서버 컨테이너 안에서 fd admin recover <login> 으로 새 코드를 받는다")
		}
		return 1
	}
	var g api.BootstrapGrant
	if err := json.Unmarshal(raw, &g); err != nil {
		fmt.Fprintf(out, "응답을 못 읽었다: %s\n", err)
		return 1
	}
	printLoginCode(out, "첫 관리자를 만들었다", g.Login, g.AccountID, g.LoginCode, g.ExpiresAt)
	return 0
}

func (a *App) runAdminRecover(ctx context.Context, args []string, out io.Writer) int {
	fs := newFlagSet("admin recover")
	dbPath := fs.String("db", "", "원장: SQLite 파일 경로 또는 Postgres 접속 URL(비면 서버와 같은 자리)")
	login, rest := splitLeadingArg(args)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	if login == "" && fs.NArg() > 0 {
		login = fs.Arg(0)
	}
	if strings.TrimSpace(login) == "" {
		fmt.Fprint(out, "복구할 관리자의 로그인 이름이 없다\n\n"+adminUsage)
		return 2
	}
	st, shown, err := openDB(a.env, a.log, *dbPath, false)
	if err != nil {
		fmt.Fprintf(out, "원장을 못 열었다: %s\n", err)
		return 1
	}
	defer st.Close()
	g, err := st.RecoverAdmin(ctx, login, time.Now())
	if err != nil {
		fmt.Fprintf(out, "복구 실패(%s): %s\n", shown, err)
		return 1
	}
	what := "관리자의 새 로그인 코드를 냈다"
	if g.Reenabled {
		what = "꺼져 있던 관리자를 다시 켜고 새 로그인 코드를 냈다"
	}
	printLoginCode(out, what, g.Login, g.AccountID, g.Code, g.ExpiresAt)
	return 0
}

// splitLeadingArg 는 첫 인자가 플래그가 아니면 떼어 낸다 - `fd admin recover <login> --db …` 꼴을 받으려고(flag 는 첫
// 위치 인자에서 멈춘다).
func splitLeadingArg(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// printLoginCode 는 로그인 코드를 한 번 보인다. 코드 줄은 따로 둔다 - 복사하기 쉽게.
func printLoginCode(out io.Writer, what, login, accountID, code string, expires time.Time) {
	fmt.Fprintf(out, "%s · %s (%s)\n\n  %s\n\n", what, login, accountID, code)
	fmt.Fprintf(out, "이 코드를 화면 /login 에 붙여 넣어라 - 한 번만 쓰고 %s 에 만료되며, 다시 보이지 않는다.\n",
		expires.UTC().Format(time.RFC3339))
}
