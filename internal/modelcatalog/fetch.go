package modelcatalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

func fetchCatalogData(ctx context.Context, client *http.Client, url, source string) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: GET %s: %s", source, url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
