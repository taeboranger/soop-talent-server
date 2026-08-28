package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// DB는 MySQL 전용의 작은 database/sql 어댑터다. 호출부는 명시적인 MySQL 문법을
// 쓰고, 과거 코드의 $1 자리표시자만 여기서 ?로 변환한다.
type DB struct{ *sql.DB }
type Result struct{ sql.Result }
type Tx struct{ *sql.Tx }
type Row struct {
	row *sql.Row
	err error
}

func (r *Row) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return r.row.Scan(dest...)
}

var dollarArg = regexp.MustCompile(`\$[0-9]+`)

// mysqlQuery는 PostgreSQL식 $1 자리표시자를 MySQL ?로 바꾼다. 같은 $1이
// 쿼리에 두 번 나오면 MySQL에는 인자도 두 번 필요하므로, 등장 순서대로 다시
// 펼친다. 예: "($1=” OR status=$1)" + ["pending"] → ["pending","pending"].
func mysqlQuery(q string, args []any) (string, []any, error) {
	matches := dollarArg.FindAllStringIndex(q, -1)
	if len(matches) == 0 {
		return q, args, nil
	}
	var b strings.Builder
	b.Grow(len(q))
	out := make([]any, 0, len(matches))
	pos := 0
	for _, m := range matches {
		b.WriteString(q[pos:m[0]])
		index, err := strconv.Atoi(q[m[0]+1 : m[1]])
		if err != nil || index < 1 || index > len(args) {
			return "", nil, fmt.Errorf("SQL 자리표시자 $%d에 대응하는 인자가 없습니다", index)
		}
		b.WriteByte('?')
		out = append(out, args[index-1])
		pos = m[1]
	}
	b.WriteString(q[pos:])
	return b.String(), out, nil
}
func (d *DB) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	q, args, err := mysqlQuery(q, args)
	if err != nil {
		return Result{}, err
	}
	r, err := d.DB.ExecContext(ctx, q, args...)
	return Result{r}, err
}
func (d *DB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	q, args, err := mysqlQuery(q, args)
	if err != nil {
		return nil, err
	}
	return d.DB.QueryContext(ctx, q, args...)
}
func (d *DB) QueryRow(ctx context.Context, q string, args ...any) *Row {
	q, args, err := mysqlQuery(q, args)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{row: d.DB.QueryRowContext(ctx, q, args...)}
}
func (d *DB) Ping(ctx context.Context) error { return d.DB.PingContext(ctx) }
func (d *DB) Begin(ctx context.Context) (*Tx, error) {
	tx, err := d.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{tx}, nil
}
func (t *Tx) Exec(ctx context.Context, q string, args ...any) (Result, error) {
	q, args, err := mysqlQuery(q, args)
	if err != nil {
		return Result{}, err
	}
	r, err := t.Tx.ExecContext(ctx, q, args...)
	return Result{r}, err
}
func (t *Tx) QueryRow(ctx context.Context, q string, args ...any) *Row {
	q, args, err := mysqlQuery(q, args)
	if err != nil {
		return &Row{err: err}
	}
	return &Row{row: t.Tx.QueryRowContext(ctx, q, args...)}
}
func (t *Tx) Commit(context.Context) error   { return t.Tx.Commit() }
func (t *Tx) Rollback(context.Context) error { return t.Tx.Rollback() }

type Store struct {
	Pool *DB
	Log  *slog.Logger
}

func Open(ctx context.Context, dsn string, log *slog.Logger) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("MySQL DSN 파싱: %w", err)
	}
	cfg.ParseTime = true
	cfg.Loc = time.Local
	cfg.MultiStatements = true
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["charset"] = "utf8mb4"
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("DB 접속 확인: %w", err)
	}
	return &Store{Pool: &DB{db}, Log: log}, nil
}
func (s *Store) Close() { s.Pool.Close() }

// Migrate는 파일별 이력을 기록한다. MySQL의 ALTER는 재실행 안전하지 않으므로
// PostgreSQL 시절의 IF NOT EXISTS 반복 실행 방식에 의존하지 않는다.
func (s *Store) Migrate(ctx context.Context, dir string) error {
	if _, err := s.Pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        name VARCHAR(255) PRIMARY KEY, applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6))`); err != nil {
		return err
	}
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
		var done int
		err := s.Pool.QueryRow(ctx, `SELECT 1 FROM schema_migrations WHERE name = $1`, f).Scan(&done)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return err
		}
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, string(b)); err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, f)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s 적용: %w", f, err)
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		s.Log.Info("마이그레이션 적용", "file", f)
	}
	return nil
}

func (s *Store) EnsureStreamer(ctx context.Context, bid, nick string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO streamers (bid, nickname) VALUES ($1, NULLIF($2,''))
        ON DUPLICATE KEY UPDATE nickname = COALESCE(NULLIF(VALUES(nickname),''), nickname)`, bid, nick)
	return err
}
func (s *Store) ChatEnabled(ctx context.Context, bid string) (bool, error) {
	var on bool
	err := s.Pool.QueryRow(ctx, `SELECT chat_enabled FROM streamers WHERE bid = $1`, bid).Scan(&on)
	if err == sql.ErrNoRows {
		return true, nil
	}
	return on, err
}
func (s *Store) SetChatEnabled(ctx context.Context, bid string, on bool) error {
	// 기존 승인 계정은 streamers 행이 없을 수 있다. 채팅 스위치는 설정
	// 자체이므로 행 유무와 관계없이 저장돼야 하며, 다음 기동 때 수집기가
	// 같은 값을 읽어 그대로 반영한다.
	_, err := s.Pool.Exec(ctx, `INSERT INTO streamers (bid, chat_enabled) VALUES ($1, $2)
        ON DUPLICATE KEY UPDATE chat_enabled=VALUES(chat_enabled)`, bid, on)
	return err
}
func (s *Store) StartBroadcast(ctx context.Context, bid, broadNo, title string) error {
	n, err := strconv.ParseInt(broadNo, 10, 64)
	if err != nil {
		return nil
	}
	_, err = s.Pool.Exec(ctx, `INSERT INTO broadcasts (broad_no,bid,title) VALUES ($1,$2,$3) ON DUPLICATE KEY UPDATE title=VALUES(title)`, n, bid, title)
	return err
}

type RawEvent struct {
	BID, BroadNo, Svc string
	Parts             []string
	Raw               []byte
	RecvAt            time.Time
}
type RawWriter struct {
	store   *Store
	in      chan RawEvent
	flushN  int
	flushAt time.Duration
	dropped int64
}

func NewRawWriter(s *Store) *RawWriter {
	return &RawWriter{store: s, in: make(chan RawEvent, 8192), flushN: 500, flushAt: 2 * time.Second}
}
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
		if len(buf) > 0 {
			if err := w.copyIn(ctx, buf); err != nil {
				w.store.Log.Error("원본 이벤트 기록 실패", "n", len(buf), "err", err)
			}
			buf = buf[:0]
		}
	}
	for {
		select {
		case <-ctx.Done():
			drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			if len(buf) > 0 {
				_ = w.copyIn(drain, buf)
			}
			cancel()
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
	tx, err := w.store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, e := range batch {
		parts, err := json.Marshal(e.Parts)
		if err != nil {
			continue
		}
		var broad any
		if n, err := strconv.ParseInt(e.BroadNo, 10, 64); err == nil {
			broad = n
		}
		if _, err = tx.Exec(ctx, `INSERT INTO raw_chat_events (bid,broad_no,svc,parts,raw,recv_at) VALUES ($1,$2,$3,$4,$5,$6)`, e.BID, broad, e.Svc, parts, e.Raw, e.RecvAt); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
func (s *Store) TouchUser(ctx context.Context, bid, userID, nick string, fanclubWeeks int) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO users (bid,user_id,nickname,fanclub_weeks) VALUES ($1,$2,NULLIF($3,''),$4)
        ON DUPLICATE KEY UPDATE nickname=COALESCE(NULLIF(VALUES(nickname),''),nickname), fanclub_weeks=GREATEST(fanclub_weeks,VALUES(fanclub_weeks)), last_seen=NOW(6)`, bid, userID, nick, fanclubWeeks)
	return err
}
