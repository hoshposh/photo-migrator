package photos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrTooLarge reports that Google rejected a file as too large for the API
// (HTTP 413). Retrying cannot help; the file must be uploaded manually.
var ErrTooLarge = errors.New("file too large for the Google Photos API")

// UploadBytes streams the raw bytes of mediaPath to the Google Photos upload
// endpoint and returns an opaque uploadToken.
// mime is sent as X-Goog-Upload-Content-Type; empty means derive it from the
// file name.
func (c *Client) UploadBytes(ctx context.Context, mediaPath, mime string) (_ string, err error) {
	if mime == "" {
		mime = mimeType(mediaPath)
	}
	f, err := os.Open(mediaPath)
	if err != nil {
		return "", fmt.Errorf("open media file: %w", err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	fi, err := f.Stat()
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/uploads", io.NopCloser(f))
	if err != nil {
		return "", err
	}
	req.ContentLength = fi.Size()
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Goog-Upload-Content-Type", mime)
	req.Header.Set("X-Goog-Upload-Protocol", "raw")

	// GetBody lets doWithRetry re-open the file on a retry.
	req.GetBody = func() (io.ReadCloser, error) {
		return os.Open(mediaPath)
	}

	resp, err := c.doWithRetry(ctx, req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		return "", fmt.Errorf("%w: byte upload HTTP 413: %s", ErrTooLarge, body)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("byte upload HTTP %d: %s", resp.StatusCode, body)
	}

	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("byte upload returned empty token")
	}
	return token, nil
}

// ─────────────────────────────────────────────────────────────
// batchCreate types
// ─────────────────────────────────────────────────────────────

type batchCreateReq struct {
	AlbumID       string         `json:"albumId,omitempty"`
	NewMediaItems []newMediaItem `json:"newMediaItems"`
}

type newMediaItem struct {
	Description     string          `json:"description,omitempty"`
	SimpleMediaItem simpleMediaItem `json:"simpleMediaItem"`
}

type simpleMediaItem struct {
	UploadToken string `json:"uploadToken"`
	FileName    string `json:"fileName"`
}

// BatchCreateResult is the per-item result from a batchCreate call.
type BatchCreateResult struct {
	UploadToken string
	MediaItemID string
	StatusCode  int
	StatusMsg   string
}

type batchCreateResp struct {
	NewMediaItemResults []struct {
		UploadToken string `json:"uploadToken"`
		Status      struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"status"`
		MediaItem struct {
			ID string `json:"id"`
		} `json:"mediaItem"`
	} `json:"newMediaItemResults"`
}

// BatchCreate submits up to 50 media items for creation in a single API call.
// albumID may be empty to upload to the library root.
func (c *Client) BatchCreate(ctx context.Context, albumID string, items []newMediaItem) ([]BatchCreateResult, error) {
	payload, err := json.Marshal(batchCreateReq{
		AlbumID:       albumID,
		NewMediaItems: items,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		c.baseURL+"/mediaItems:batchCreate",
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

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("batchCreate HTTP %d: %s", resp.StatusCode, respBody)
	}

	var bcResp batchCreateResp
	if err := json.Unmarshal(respBody, &bcResp); err != nil {
		return nil, fmt.Errorf("decode batchCreate response: %w", err)
	}

	results := make([]BatchCreateResult, len(bcResp.NewMediaItemResults))
	for i, r := range bcResp.NewMediaItemResults {
		res := BatchCreateResult{
			UploadToken: r.UploadToken,
			MediaItemID: r.MediaItem.ID,
			StatusCode:  r.Status.Code,
			StatusMsg:   r.Status.Message,
		}
		if r.Status.Code != 0 {
			res.StatusMsg = r.Status.Message + " (code " + strconv.Itoa(r.Status.Code) + ")"
		}
		results[i] = res
	}
	return results, nil
}

// UploadMedia is a convenience wrapper that executes the complete two-step
// upload dance (byte upload → batchCreate) for a single media file.
//
// Returns the Google Photos media item ID on success.
//
// fileName and mime override the name and MIME type sent to Google; empty
// means derive them from mediaPath.
func (c *Client) UploadMedia(ctx context.Context, mediaPath, fileName, mime, description, albumID string) (string, error) {
	if fileName == "" {
		fileName = filepath.Base(mediaPath)
	}
	// Step 1: byte upload.
	token, err := c.UploadBytes(ctx, mediaPath, mime)
	if err != nil {
		return "", fmt.Errorf("byte upload: %w", err)
	}

	// Step 2: batchCreate (single item).
	results, err := c.BatchCreate(ctx, albumID, []newMediaItem{
		{
			Description: description,
			SimpleMediaItem: simpleMediaItem{
				UploadToken: token,
				FileName:    fileName,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("batchCreate: %w", err)
	}
	if len(results) == 0 {
		return "", fmt.Errorf("batchCreate returned empty results")
	}
	if results[0].StatusCode != 0 {
		return "", fmt.Errorf("batchCreate item error: %s", results[0].StatusMsg)
	}
	return results[0].MediaItemID, nil
}

// mimeType returns the MIME type for a media file based on its extension.
func mimeType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".heic", ".heif":
		return "image/heic"
	// RAW formats
	case ".arw":
		return "image/x-sony-arw"
	case ".dng":
		return "image/x-adobe-dng"
	case ".cr2":
		return "image/x-canon-cr2"
	case ".cr3":
		return "image/x-canon-cr3"
	case ".nef":
		return "image/x-nikon-nef"
	case ".raf":
		return "image/x-fuji-raf"
	case ".orf":
		return "image/x-olympus-orf"
	case ".rw2":
		return "image/x-panasonic-rw2"
	// Video
	case ".mp4":
		return "video/mp4"
	case ".mov":
		return "video/quicktime"
	case ".avi":
		return "video/x-msvideo"
	case ".m4v":
		return "video/x-m4v"
	case ".3gp":
		return "video/3gpp"
	case ".mkv":
		return "video/x-matroska"
	case ".wmv":
		return "video/x-ms-wmv"
	default:
		return "application/octet-stream"
	}
}
