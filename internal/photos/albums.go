package photos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Album represents a Google Photos album.
type Album struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// CreateAlbum creates a new album in the destination Google Photos account
// and returns its metadata.
//
// Calls are serialised with other write calls (see Client.writeMu).
func (c *Client) CreateAlbum(ctx context.Context, title string) (*Album, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	payload, err := json.Marshal(map[string]any{
		"album": map[string]string{"title": title},
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		c.baseURL+"/albums",
		bytes.NewReader(payload),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}

	resp, err := c.doWithRetry(ctx, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("createAlbum HTTP %d: %s", resp.StatusCode, body)
	}

	var album Album
	if err := json.Unmarshal(body, &album); err != nil {
		return nil, fmt.Errorf("decode album response: %w", err)
	}
	return &album, nil
}
