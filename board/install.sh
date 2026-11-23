#!/usr/bin/env bash
# 보드를 이 맥의 launchd 서비스로 올린다(다시 부르면 새 설정 · 새 경로로 다시 올린다).
#
#   board/install.sh            plist 를 쓰고 bootout → bootstrap, /healthz 가 200 이면 BOARD_READY 한 줄
#   board/install.sh --print    plist 만 stdout 으로(아무것도 안 바꾼다)
#   board/install.sh --uninstall
#
# 설정은 환경 변수로 받아 plist 에 적는다(README 「설정」). 안 주면 지금 돌고 있는 plist 의 값을 이어받고,
# 그것도 없으면 기본값이다 — 다시 설치할 때 기계별 값을 잃지 않게.
#   BOARD_PORT(7430) · BOARD_PROJECT · BOARD_FILE_ROOTS · BOARD_LINKS · BOARD_HOSTS · FD_CONFIG
#   BOARD_TS_PORT(37430): BOARD_HOSTS 가 비면 tailscale 이름과 이 포트로 「<이름>:<포트>」를 만든다
#   BOARD_LABEL(com.<사용자>.flightdeck-board) · BOARD_PYTHON(/usr/bin/python3)
#
# tailscale serve 는 건드리지 않는다(공유 설정). 경로가 없으면 칠 명령만 알려 준다.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
label="${BOARD_LABEL:-com.$(id -un).flightdeck-board}"
plist="$HOME/Library/LaunchAgents/$label.plist"
py="${BOARD_PYTHON:-/usr/bin/python3}"
log="$HOME/Library/Logs/flightdeck-board.log"
mode="${1:-install}"

if [ "$(uname -s)" != "Darwin" ]; then
  echo "BOARD_INSTALL_REFUSED: launchd 는 macOS 만. 다른 OS 는 손으로: $py -I $here/board.py (환경 변수는 README 「설정」)" >&2
  exit 2
fi

if [ "$mode" = "--uninstall" ]; then
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  rm -f "$plist"
  echo "BOARD_UNINSTALLED $label"
  exit 0
fi

# 지금 plist 의 값(있으면) -> 안 준 변수의 기본.
prev() { [ -f "$plist" ] && /usr/libexec/PlistBuddy -c "Print :EnvironmentVariables:$1" "$plist" 2>/dev/null || true; }
port="${BOARD_PORT:-$(prev BOARD_PORT)}"; port="${port:-7430}"
project="${BOARD_PROJECT-$(prev BOARD_PROJECT)}"
roots="${BOARD_FILE_ROOTS:-$(prev BOARD_FILE_ROOTS)}"
links="${BOARD_LINKS:-$(prev BOARD_LINKS)}"
fdcfg="${FD_CONFIG:-$(prev FD_CONFIG)}"
hosts="${BOARD_HOSTS:-$(prev BOARD_HOSTS)}"
if [ -z "$hosts" ] && command -v tailscale >/dev/null 2>&1; then
  ts_name="$(tailscale status --json 2>/dev/null | "$py" -I -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))' 2>/dev/null || true)"
  [ -n "$ts_name" ] && hosts="$ts_name:${BOARD_TS_PORT:-37430}"
fi

render() {
  LABEL="$label" PY="$py" BOARD="$here/board.py" LOG="$log" \
  E_BOARD_PORT="$port" E_BOARD_PROJECT="$project" E_BOARD_FILE_ROOTS="$roots" E_BOARD_LINKS="$links" \
  E_BOARD_HOSTS="$hosts" E_FD_CONFIG="$fdcfg" \
  "$py" -I -c '
import os, plistlib, sys
env = {k[2:]: v for k, v in os.environ.items() if k.startswith("E_") and v}
sys.stdout.buffer.write(plistlib.dumps({
    "Label": os.environ["LABEL"],
    "ProgramArguments": [os.environ["PY"], "-I", os.environ["BOARD"]],
    "EnvironmentVariables": env,
    "RunAtLoad": True, "KeepAlive": True, "ThrottleInterval": 10,
    "StandardErrorPath": os.environ["LOG"], "StandardOutPath": os.environ["LOG"],
}))'
}

if [ "$mode" = "--print" ]; then
  render
  exit 0
fi

mkdir -p "$(dirname "$plist")" "$(dirname "$log")"
tmp="$(mktemp "$plist.XXXXXX")"
render >"$tmp"
plutil -lint "$tmp" >/dev/null
mv "$tmp" "$plist"
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
# bootout 은 비동기라 곧바로 bootstrap 하면 「이미 있음」(5)으로 진다. 빠질 때까지 잠깐 기다린다.
for _ in $(seq 1 50); do launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 || break; sleep 0.1; done
launchctl bootstrap "gui/$(id -u)" "$plist"

for _ in $(seq 1 50); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/healthz" || true)"
  [ "$code" = "200" ] && break
  sleep 0.2
done
if [ "$code" != "200" ]; then
  echo "BOARD_INSTALL_FAILED: http://127.0.0.1:$port/healthz = $code (로그 $log, fd 서버가 떠 있는지 curl -s localhost:7420/healthz)" >&2
  tail -5 "$log" >&2 || true
  exit 1
fi
echo "BOARD_READY http://127.0.0.1:$port label=$label board=$here/board.py hosts=${hosts:-(루프백만)}"
if [ -n "$hosts" ] && command -v tailscale >/dev/null 2>&1; then
  ts_port="${hosts##*:}"
  if ! tailscale serve status 2>/dev/null | grep -q ":$ts_port"; then
    echo "tailnet 경로 없음: tailscale serve --bg --https=$ts_port http://127.0.0.1:$port 로 연다(다른 경로는 그대로 둔다)"
  fi
fi
