// SoopTalent 수집 서버.
//
// 스트리머는 OAuth로 로그인하고 자기 SOOP 아이디를 신청한다.
// 관리자가 승인하면 그때부터 해당 채널의 채팅 수집이 시작된다.
//
// 환경변수:
//
//	DATABASE_URL    DB DSN (필수)
//	ADMIN_SOOP_IDS  SOOP OAuth 로그인 뒤 관리자 권한을 줄 SOOP ID. 쉼표 구분.
//	LISTEN          HTTP 바인드 주소 (기본 :8080)
//	MIGRATIONS      마이그레이션 디렉터리 (기본 ./migrations)
//	LOG_LEVEL       debug|info|warn|error (기본 info)
//	STREAMERS       (선택) 승인 절차 없이 강제로 수집할 아이디. 개발용.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sooptalent/server/internal/api"
	"github.com/sooptalent/server/internal/auth"
	"github.com/sooptalent/server/internal/collector"
	"github.com/sooptalent/server/internal/oauth"
	"github.com/sooptalent/server/internal/soop"
	"github.com/sooptalent/server/internal/store"
)

func main() {
	log := newLogger()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Error("DATABASE_URL이 설정되지 않았습니다")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, dsn, log)
	if err != nil {
		log.Error("DB 준비 실패", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	migDir := envOr("MIGRATIONS", "./migrations")
	if err := st.Migrate(ctx, migDir); err != nil {
		log.Error("마이그레이션 실패", "dir", migDir, "err", err)
		os.Exit(1)
	}

	configureAdmins(ctx, st, log)

	raw := store.NewRawWriter(st)
	rawDone := make(chan struct{})
	go func() { defer close(rawDone); raw.Run(ctx) }()

	col := collector.New(st, raw, log)

	// 승인된 스트리머만 수집한다. 승인/철회는 관리자 화면에서 즉시 반영된다.
	approved, err := auth.ApprovedBIDs(ctx, st.Pool)
	if err != nil {
		log.Error("승인 목록 조회 실패", "err", err)
	}
	for _, bid := range approved {
		if err := st.EnsureStreamer(ctx, bid, ""); err != nil {
			log.Warn("스트리머 등록 실패", "bid", bid, "err", err)
			continue
		}
		col.Add(ctx, bid)
	}

	// 개발 편의: 승인 절차 없이 특정 채널을 수집한다.
	for _, bid := range splitCSV(os.Getenv("STREAMERS")) {
		if err := st.EnsureStreamer(ctx, bid, ""); err == nil {
			col.Add(ctx, bid)
			log.Warn("STREAMERS로 강제 수집 (승인 절차 없음)", "bid", bid)
		}
	}

	go purgeSessionsLoop(ctx, st, log)

	listen := envOr("LISTEN", ":8080")
	// PUBLIC_URL은 소셜 로그인 콜백 주소를 만드는 데 쓴다.
	// 각 제공자 콘솔에 {PUBLIC_URL}/api/oauth/{provider}/callback 을 등록해야 한다.
	publicURL := envOr("PUBLIC_URL", "http://localhost"+listen)
	reg := oauth.NewRegistry(publicURL)
	if _, ok := reg.Config(oauth.Soop); ok && !reg.SOOPTokenStorageReady() {
		log.Error("SOOP_TOKEN_ENCRYPTION_KEY가 없어 공식 채팅 OAuth 토큰을 저장할 수 없습니다")
	}
	if enabled := reg.Enabled(); len(enabled) > 0 {
		for _, p := range enabled {
			log.Info("소셜 로그인 활성", "provider", p, "callback", reg.CallbackURL(p))
		}
	} else {
		log.Warn("소셜 로그인 미설정 — OAuth 제공자를 하나 이상 설정해야 로그인할 수 있습니다")
	}

	srv := &http.Server{
		Addr: listen,
		Handler: (&api.Server{
			Store: st, Log: log, Streamer: col, OAuth: reg, SoopAPI: soop.NewClient(),
			StreamerExtensionInstallURL: envOr("STREAMER_EXTENSION_INSTALL_URL", "https://chromewebstore.google.com/detail/ockjgoalmoffhfphpdijdlkmcafkkgln"),
		}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("HTTP 수신 대기", "addr", srv.Addr, "admin", strings.TrimRight(publicURL, "/")+"/admin")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 서버 종료", "err", err)
		}
	}()

	log.Info("수집 서버 기동", "승인된_스트리머", len(approved))

	<-ctx.Done()
	log.Info("종료 신호 수신 — 버퍼를 비웁니다")

	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	_ = srv.Shutdown(shutCtx)
	cancel()

	select {
	case <-rawDone:
	case <-time.After(15 * time.Second):
		log.Warn("버퍼 정리 시간 초과")
	}
	log.Info("종료 완료")
}

// configureAdmins는 ADMIN_SOOP_IDS에 든 SOOP ID로 공식 OAuth 로그인한 계정에
// 관리자 권한을 준다. 계정을 미리 만들지 않으므로 별도 관리자 가입은 없다.
func configureAdmins(ctx context.Context, st *store.Store, log *slog.Logger) {
	ids := splitCSV(os.Getenv("ADMIN_SOOP_IDS"))
	auth.SetAdminSOOPIDs(ids)
	if len(ids) == 0 {
		log.Warn("관리자 SOOP ID가 없습니다. ADMIN_SOOP_IDS를 설정하세요")
		return
	}
	// 이미 로그인해 둔 계정도 다음 로그인까지 기다리지 않도록 바로 승격한다.
	// ID 값은 운영자가 정한 환경변수이고, 브라우저 요청값은 여기 쓰이지 않는다.
	for _, id := range ids {
		if _, err := st.Pool.Exec(ctx, `
			UPDATE accounts a JOIN oauth_identities o ON o.account_id = a.id
			   SET a.is_admin = TRUE
			 WHERE o.provider = 'soop' AND lower(o.provider_user_id) = lower($1)`, id); err != nil {
			log.Warn("관리자 SOOP ID 적용 실패", "err", err)
		}
	}
	log.Info("OAuth 관리자 SOOP ID 설정", "count", len(ids))
}

// purgeSessionsLoop은 만료된 세션을 주기적으로 지운다.
func purgeSessionsLoop(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := auth.PurgeExpired(ctx, st.Pool); err == nil && n > 0 {
				log.Info("만료 세션 정리", "count", n)
			}
		}
	}
}

// openStore는 DB가 아직 뜨지 않았을 수 있으므로 잠시 재시도한다.
func openStore(ctx context.Context, dsn string, log *slog.Logger) (*store.Store, error) {
	var lastErr error
	for i := range 10 {
		st, err := store.Open(ctx, dsn, log)
		if err == nil {
			return st, nil
		}
		lastErr = err
		log.Warn("DB 접속 재시도", "attempt", i+1, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, lastErr
}

func newLogger() *slog.Logger {
	lv := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
