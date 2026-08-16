// sooptalent-migrate는 배포 전에 DB 스키마를 최신 마이그레이션까지 올리고 끝난다.
// Argo CD PreSync Job에서만 사용하며, HTTP 서버나 채팅 수집기를 시작하지 않는다.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/sooptalent/server/internal/store"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		slog.Error("DATABASE_URL이 설정되지 않았습니다")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	st, err := store.Open(ctx, dsn, slog.Default())
	if err != nil {
		slog.Error("DB 접속 실패", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	migrations := os.Getenv("MIGRATIONS")
	if migrations == "" {
		migrations = "./migrations"
	}
	if err := st.Migrate(ctx, migrations); err != nil {
		slog.Error("마이그레이션 실패", "dir", migrations, "err", err)
		os.Exit(1)
	}
	slog.Info("마이그레이션 완료", "dir", migrations)
}
