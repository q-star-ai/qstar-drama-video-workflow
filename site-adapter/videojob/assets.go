package videojob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// AssetFetcher downloads a private asset and proves it is the approved
// version by its sha256.
type AssetFetcher struct {
	// Sign turns a private asset URI into a short-lived download URL. The
	// signed URL is used once and never stored.
	Sign   func(uri string) (string, error)
	Client *http.Client
	// MaxBytes bounds a single download (default 200 MB).
	MaxBytes int64
}

func (f *AssetFetcher) open(ctx context.Context, ref AssetRef) (io.ReadCloser, error) {
	url := ref.URI
	if f.Sign != nil {
		signed, err := f.Sign(ref.URI)
		if err != nil {
			return nil, fmt.Errorf("sign %s: %w", ref.AssetID, err)
		}
		url = signed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", ref.AssetID, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("fetch %s: HTTP %d", ref.AssetID, resp.StatusCode)
	}
	return resp.Body, nil
}

// copyVerified writes the asset to w and fails unless its sha256 matches.
func (f *AssetFetcher) copyVerified(ctx context.Context, ref AssetRef, w io.Writer) error {
	body, err := f.open(ctx, ref)
	if err != nil {
		return err
	}
	defer body.Close()
	limit := f.MaxBytes
	if limit <= 0 {
		limit = 200 << 20
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(body, limit+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", ref.AssetID, err)
	}
	if n > limit {
		return fmt.Errorf("asset %s exceeds %d bytes", ref.AssetID, limit)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != ref.SHA256 {
		return &ProviderError{ClassInvalidInput, fmt.Sprintf("asset %s version %s checksum %s does not match approved %s", ref.AssetID, ref.Version, got, ref.SHA256)}
	}
	return nil
}

// Verify checks every reference against its approved checksum.
func (f *AssetFetcher) Verify(ctx context.Context, refs []AssetRef) error {
	for _, ref := range refs {
		if err := f.copyVerified(ctx, ref, io.Discard); err != nil {
			return err
		}
	}
	return nil
}

// Download saves a verified asset under dir and returns its absolute path.
func (f *AssetFetcher) Download(ctx context.Context, ref AssetRef, dir, name string) (string, error) {
	path, err := filepath.Abs(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if err := f.copyVerified(ctx, ref, out); err != nil {
		out.Close()
		os.Remove(path)
		return "", err
	}
	return path, out.Close()
}
