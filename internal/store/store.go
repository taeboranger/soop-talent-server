package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
}

func Open(ctx context.Context, dsn string, log *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("DSN 파싱: %w", err)
	}
	// 수집기 고루틴이 많아도 커넥션은 소수로 충분하다.
	// 쓰기는 전부 배치로 모아 나가므로 동시 쿼리 수가 적다.
	cfg.MaxConns = 12
	cfg.MinConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("DB 접속 확인: %w", err)
	}
	return &Store{Pool: pool, Log: log}, nil
}

func (s *Store) Close() { s.Pool.Close() }

// Migrate는 migrations 디렉터리의 .sql 파일을 이름순으로 적용한다.
// 모든 문장이 IF NOT EXISTS이므로 반복 실행해도 안전하다.
func (s *Store) Migrate(ctx context.Context, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("마이그레이션 디렉터리: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return err
		}
		if _, err := s.Pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("%s 적용: %w", f, err)
		}
		s.Log.Info("마이그레이션 적용", "file", f)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 스트리머 / 방송
// ---------------------------------------------------------------------------

func (s *Store) EnsureStreamer(ctx context.Context, bid, nick string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO streamers (bid, nickname) VALUES ($1, NULLIF($2,''))
		ON CONFLICT (bid) DO UPDATE
		   SET nickname = COALESCE(NULLIF(EXCLUDED.nickname,''), streamers.nickname)`,
		bid, nick)
	return err
}

// ChatEnabled는 채팅 연동 스위치를 읽는다.
// 아직 등록되지 않은 방송국은 켜진 것으로 본다 — 승인 직후의 기본값과 같다.
func (s *Store) ChatEnabled(ctx context.Context, bid string) (bool, error) {
	var on bool
	err := s.Pool.QueryRow(ctx,
		`SELECT chat_enabled FROM streamers WHERE bid = $1`, bid).Scan(&on)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	return on, err
}

// SetChatEnabled는 스위치를 저장한다. 서버를 다시 켜도 이 값이 유지된다.
func (s *Store) SetChatEnabled(ctx context.Context, bid string, on bool) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE streamers SET chat_enabled = $2 WHERE bid = $1`, bid, on)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("등록되지 않은 방송국입니다: %s", bid)
	}
	return nil
}

func (s *Store) StartBroadcast(ctx context.Context, bid, broadNo, title string) error {
	n, err := strconv.ParseInt(broadNo, 10, 64)
	if err != nil {
		return nil // 방송 번호가 없으면 기록할 것이 없다
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO broadcasts (broad_no, bid, title) VALUES ($1, $2, $3)
		ON CONFLICT (broad_no) DO UPDATE SET title = EXCLUDED.title`,
		n, bid, title)
	return err
}

// ---------------------------------------------------------------------------
// 원본 이벤트 배치 기록
// ---------------------------------------------------------------------------

type RawEvent struct {
	BID     string
	BroadNo string
	Svc     string
	Parts   []string
	Raw     []byte
	RecvAt  time.Time
}

// RawWriter는 원본 패킷을 모아 COPY로 일괄 기록한다.
// 대형 방송은 초당 100+ 패킷을 만들므로 건건이 INSERT하면 DB가 먼저 무너진다.
type RawWriter struct {
	store   *Store
	in      chan RawEvent
	flushN  int
	flushAt time.Duration
	dropped int64
}

func NewRawWriter(s *Store) *RawWriter {
	return &RawWriter{
		store:   s,
		in:      make(chan RawEvent, 8192),
		flushN:  500,
		flushAt: 2 * time.Second,
	}
}

// Write는 절대 블로킹하지 않는다.
// 큐가 가득 차면 원본 로그를 버린다 — 수집 지연이 채팅 소켓 읽기를 막는 것보다 낫다.
func (w *RawWriter) Write(e RawEvent) {
	select {
	case w.in <- e:
	default:
		w.dropped++
	}
}

func (w *RawWriter) Run(ctx context.Context) {
	buf := make([]RawEvent, 0, w.flushN)
	t := time.NewTicker(w.flushAt)
	defer t.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := w.copyIn(ctx, buf); err != nil {
			w.store.Log.Error("원본 이벤트 기록 실패", "n", len(buf), "err", err)
		}
		buf = buf[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// 종료 시 남은 것을 밀어낸다. ctx가 이미 취소됐으므로 새 컨텍스트를 쓴다.
			drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if len(buf) > 0 {
				if err := w.copyIn(drain, buf); err != nil {
					w.store.Log.Error("종료 시 기록 실패", "err", err)
				}
			}
			cancel()
			if w.dropped > 0 {
				w.store.Log.Warn("원본 이벤트 유실", "count", w.dropped)
			}
			return
		case e := <-w.in:
			buf = append(buf, e)
			if len(buf) >= w.flushN {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

func (w *RawWriter) copyIn(ctx context.Context, batch []RawEvent) error {
	rows := make([][]any, 0, len(batch))
	for _, e := range batch {
		parts, err := json.Marshal(e.Parts)
		if err != nil {
			continue
		}
		var broadNo *int64
		if n, err := strconv.ParseInt(e.BroadNo, 10, 64); err == nil {
			broadNo = &n
		}
		rows = append(rows, []any{e.BID, broadNo, e.Svc, parts, e.Raw, e.RecvAt})
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := w.store.Pool.CopyFrom(ctx,
		pgx.Identifier{"raw_chat_events"},
		[]string{"bid", "broad_no", "svc", "parts", "raw", "recv_at"},
		pgx.CopyFromRows(rows))
	return err
}

// ---------------------------------------------------------------------------
// 시청자
// ---------------------------------------------------------------------------

// TouchUser는 시청자를 기록하고 팬클럽 주차를 갱신한다.
// fanclubWeeks가 -1이면 미가입 상태이므로 기존 값을 덮어쓰지 않는다.
func (s *Store) TouchUser(ctx context.Context, bid, userID, nick string, fanclubWeeks int) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO users (bid, user_id, nickname, fanclub_weeks)
		VALUES ($1, $2, NULLIF($3,''), $4)
		ON CONFLICT (bid, user_id) DO UPDATE
		   SET nickname      = COALESCE(NULLIF(EXCLUDED.nickname,''), users.nickname),
		       fanclub_weeks = GREATEST(users.fanclub_weeks, EXCLUDED.fanclub_weeks),
		       last_seen     = now()`,
		bid, userID, nick, fanclubWeeks)
	return err
}
