package shop

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sooptalent/server/internal/apperr"
)

// MaxImageBytes는 보상 미리보기 한 장의 상한이다.
// 목록에 여러 장이 함께 뜨므로 크게 받을 이유가 없다.
const MaxImageBytes = 2 << 20 // 2 MiB

// Image는 저장된 미리보기 한 장이다.
type Image struct {
	ID   int64
	Mime string
	Data []byte
}

// 매직 바이트로 실제 형식을 확인한다.
//
// 클라이언트가 보낸 Content-Type은 믿지 않는다. 우리 도메인에서 그대로 돌려주는
// 파일이므로, HTML이나 SVG가 이미지인 척 올라오면 그 자체가 XSS 통로가 된다.
// SVG는 스크립트를 품을 수 있어 아예 받지 않는다.
var imageSignatures = []struct {
	mime   string
	prefix []byte
}{
	{"image/png", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}},
	{"image/jpeg", []byte{0xFF, 0xD8, 0xFF}},
	{"image/gif", []byte("GIF87a")},
	{"image/gif", []byte("GIF89a")},
}

// SniffImage는 바이트를 보고 형식을 판정한다. 이미지가 아니면 오류다.
func SniffImage(b []byte) (string, error) {
	if len(b) == 0 {
		return "", apperr.New("빈 파일입니다.")
	}
	if len(b) > MaxImageBytes {
		return "", apperr.Newf("이미지는 %dMB 이하만 올릴 수 있습니다.", MaxImageBytes>>20)
	}
	for _, s := range imageSignatures {
		if bytes.HasPrefix(b, s.prefix) {
			return s.mime, nil
		}
	}
	// WebP: "RIFF" + 크기 4바이트 + "WEBP"
	if len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")) {
		return "image/webp", nil
	}
	return "", apperr.New("PNG, JPEG, GIF, WebP 이미지만 올릴 수 있습니다.")
}

// SaveImage는 미리보기를 저장하고 식별자를 돌려준다.
func SaveImage(ctx context.Context, pool *pgxpool.Pool, bid string, data []byte) (*Image, error) {
	mime, err := SniffImage(data)
	if err != nil {
		return nil, err
	}
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO shop_images (bid, mime, bytes, byte_size)
		VALUES ($1, $2, $3, $4) RETURNING id`,
		bid, mime, data, len(data)).Scan(&id); err != nil {
		return nil, fmt.Errorf("이미지 저장: %w", err)
	}
	return &Image{ID: id, Mime: mime, Data: data}, nil
}

// GetImage는 저장된 미리보기를 읽는다.
// 시청자 확장도 봐야 하므로 소유자로 좁히지 않는다 —
// 열린 보상 페이지의 이미지는 어차피 공개 정보다.
func GetImage(ctx context.Context, pool *pgxpool.Pool, id int64) (*Image, error) {
	var img Image
	img.ID = id
	err := pool.QueryRow(ctx,
		`SELECT mime, bytes FROM shop_images WHERE id = $1`, id).Scan(&img.Mime, &img.Data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &img, err
}

// PurgeOrphanImages는 어떤 보상도 참조하지 않는 이미지를 지운다.
// 편집 중 올렸다가 저장하지 않은 이미지가 쌓이는 것을 막는다.
func PurgeOrphanImages(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM shop_images i
		 WHERE i.created_at < now() - interval '1 day'
		   AND NOT EXISTS (SELECT 1 FROM shop_items s WHERE s.image_id = i.id)`)
	return tag.RowsAffected(), err
}
