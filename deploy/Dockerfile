# flightdeck Postgres 겹의 이미지 - compose.postgres.yaml 의 db · db-roles · db-backup 이 함께 쓴다.
#
# ★ 이 겹이 읽는 파일(접속 규칙 · 계정 스크립트 · 덤프 스크립트)을 이미지에 굽는다. 판 디렉토리(플러그인 설치본
#   …/flightdeck/<버전>)에서 bind 하지 않는다: db · db-backup 은 restart: unless-stopped 라 재부팅 · 크래시 뒤 Docker 가 만든 때의
#   마운트로 다시 띄우는데, 그 판 디렉토리가 정리로 지워졌으면 bind 의 원천이 없어 뜨지 못한다(원장 DB 가 안 뜨면 서버도 안 뜬다).
#   이미지는 컨테이너가 붙들고 있는 동안 지워지지 않는다.
# ★ 모드를 COPY 가 정한다(--chmod). 체크아웃 · 설치본의 모드(umask 077 이면 0600)를 그대로 옮기면 db 의 postgres 사용자(이미지의
#   uid)와 FD_UID 로 도는 db-roles · db-backup 이 root 소유 파일을 못 읽는다. 스크립트는 sh 로 부르므로 실행 비트가 없어도 된다.
# ★ 주판은 시험 DB(compose.test.yaml 의 pg-test)와 같다 - 시험(TestComposePostgresImagesMatchTheTestServer)이 이 FROM 과 맞댄다.
FROM postgres:17
COPY --chmod=0644 pg_hba.conf /etc/postgresql/fd_hba.conf
COPY --chmod=0644 db-roles.sh /usr/local/bin/fd-db-roles
COPY --chmod=0644 db-backup.sh /usr/local/bin/fd-db-backup
