package soop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// 방송국 게시판 API. 실측으로 확인한 엔드포인트다 (2026-08-10).
//
//	GET /v1.1/channel/{bid}/post/{titleNo}
//	GET /v1.1/channel/{bid}/post/{titleNo}/comment?page=1&orderBy=reg_date&cCommentNo=0
const channelAPI = "https://api-channel.sooplive.com/v1.1/channel"

type Post struct {
	TitleNo   int64  `json:"titleNo"`
	TitleName string `json:"titleName"`
	UserID    string `json:"userId"`
	UserNick  string `json:"userNick"`
	BbsNo     int64  `json:"bbsNo"`
}

type Comment struct {
	CommentNo int64  `json:"pCommentNo"`
	TitleNo   int64  `json:"titleNo"`
	UserID    string `json:"userId"`
	UserNick  string `json:"userNick"`
	Comment   string `json:"comment"`
	RegDate   string `json:"regDate"` // "2026-07-21 09:57:32"
}

// RegTime은 SOOP이 주는 문자열 시각을 파싱한다. 실패하면 영시각을 돌려준다.
func (c Comment) RegTime() time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", c.RegDate, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Post는 게시글 정보를 가져온다. 작성자 확인에 쓴다.
func (c *Client) Post(ctx context.Context, bid string, titleNo int64) (*Post, error) {
	var wrap struct {
		Data Post `json:"data"`
	}
	url := fmt.Sprintf("%s/%s/post/%d", channelAPI, bid, titleNo)
	if err := c.getJSON(ctx, url, &wrap); err != nil {
		return nil, err
	}
	if wrap.Data.TitleNo == 0 {
		// data로 감싸지 않고 바로 돌려주는 경우도 있어 한 번 더 시도한다.
		var direct Post
		if err := c.getJSON(ctx, url, &direct); err == nil && direct.TitleNo != 0 {
			return &direct, nil
		}
		return nil, fmt.Errorf("게시글을 찾을 수 없습니다 (%d)", titleNo)
	}
	return &wrap.Data, nil
}

// Comments는 게시글의 댓글을 전부 가져온다.
// 페이지네이션을 끝까지 따라가되, 응답이 이상하면 무한 루프에 빠지지 않도록 상한을 둔다.
func (c *Client) Comments(ctx context.Context, bid string, titleNo int64) ([]Comment, error) {
	const maxPages = 200

	var out []Comment
	seen := make(map[int64]bool)

	for page := 1; page <= maxPages; page++ {
		var wrap struct {
			Data []Comment `json:"data"`
			Meta struct {
				LastPage int `json:"last_page"`
			} `json:"meta"`
		}
		url := fmt.Sprintf("%s/%s/post/%d/comment?page=%d&orderBy=reg_date&cCommentNo=0",
			channelAPI, bid, titleNo, page)
		if err := c.getJSON(ctx, url, &wrap); err != nil {
			return out, err
		}
		if len(wrap.Data) == 0 {
			break
		}
		added := 0
		for _, cm := range wrap.Data {
			if cm.CommentNo == 0 || seen[cm.CommentNo] {
				continue
			}
			seen[cm.CommentNo] = true
			out = append(out, cm)
			added++
		}
		// 새 댓글이 하나도 없으면 같은 페이지를 반복해서 받고 있는 것이다.
		if added == 0 {
			break
		}
		if wrap.Meta.LastPage > 0 && page >= wrap.Meta.LastPage {
			break
		}
	}
	return out, nil
}

func (c *Client) getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://www.sooplive.com/")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("게시판 API 오류 (HTTP %d)", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}
