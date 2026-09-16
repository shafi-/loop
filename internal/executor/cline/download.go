// Small shared helpers for the cline executor's install surface.
package cline

import (
	"fmt"
	"io"
	"net/http"
	"os"
)

// usableExecutable reports a regular, executable file.
func usableExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// download fetches url into a local file (streamed; bun's zip is ~30 MB).
func download(url, dest string) error {
	resp, err := http.Get(url) //nolint:gosec — fixed release URL host
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}
