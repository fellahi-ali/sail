package main

import (
	"context"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"go.coder.com/flog"
	"go.coder.com/sail/internal/codeserver"
	"golang.org/x/xerrors"
)

// loadCodeServer produces a path containing the code-server binary.
// It will attempt to cache the binary.
func loadCodeServer(ctx context.Context) (string, error) {
	start := time.Now()

	const cachePath = "/tmp/sail-code-server-cache/code-server"

	// downloadURLPath stores the download URL, so we know whether we should update
	// the binary.
	downloadURLPath := cachePath + ".download_url"

	// Only check for a new codeserver if it's over an hour old.
	info, err := os.Stat(cachePath)
	if err == nil {
		if info.ModTime().Add(time.Hour).After(time.Now()) {
			return cachePath, nil
		}
	}

	err = os.MkdirAll(filepath.Dir(cachePath), 0750)
	if err != nil {
		return "", err
	}

	_, err = os.Stat(cachePath)
	if err != nil && !os.IsNotExist(err) {
		return "", xerrors.Errorf("failed to stat %v: %v", cachePath, err)
	}

	cachedBinExists := err == nil

	fi, err := os.OpenFile(cachePath, os.O_CREATE|os.O_RDWR, 0750)
	if err != nil {
		return "", err
	}
	defer fi.Close()

	downloadURL, err := codeserver.DownloadURL(ctx)
	if err != nil {
		return "", err
	}

	lastDownloadURL, err := ioutil.ReadFile(downloadURLPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", xerrors.Errorf("failed to get download URL: %w", err)
		
		}

		lastDownloadURL = []byte("")
	}

	// The binary is already up to date.
	if string(lastDownloadURL) == downloadURL && cachedBinExists {
		return cachePath, nil
	}

	tarFi, err := http.Get(downloadURL)
	if err != nil {
		_ = os.Remove(cachePath)
		return "", xerrors.Errorf("failed to get %v: %w", downloadURL, err)
	}
	defer tarFi.Body.Close()

	binRd, err := codeserver.Extract(ctx, tarFi.Body)
	if err != nil {
		_ = os.Remove(cachePath)
		return "", xerrors.Errorf("failed to untar %v: %w", downloadURL, err)
	}

	_, err = io.Copy(fi, binRd)
	if err != nil {
		_ = os.Remove(cachePath)
		return "", xerrors.Errorf("failed to copy binary into %v: %w", cachePath, err)
	}

	err = fi.Close()
	if err != nil {
		return "", xerrors.Errorf("failed to close %v: %v", fi.Name(), err)
	}

	err = ioutil.WriteFile(downloadURLPath, []byte(downloadURL), 0640)
	if err != nil {
		return "", err
	}

	flog.Info("loaded code-server in %v", time.Since(start))

	return cachePath, nil
}

// codeServerPort gets the port of the running code-server binary.
//
// It will retry for 5 seconds if we fail to find the port in case
// the code-server binary is still starting up.
func codeServerPort(cntName string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()

	var (
		port string
		err  error
	)

	for ctx.Err() == nil {
		if runtime.GOOS == "darwin" {
			// macOS uses port forwarding instead of host networking so netstat stuff below will not work
			// as it will find the port inside the container, which we already know is 8443.
			cmd := exec.CommandContext(ctx, "docker", "port", cntName, "8443")
			var out []byte
			out, err = cmd.CombinedOutput()
			if err != nil {
				continue
			}

			addr := strings.TrimSpace(string(out))
			_, port, err = net.SplitHostPort(addr)
			if err != nil {
				return "", xerrors.Errorf("invalid address from docker port: %q", string(out))
			}
		} else {
			port, err = codeserver.Port(cntName)
			if xerrors.Is(err, codeserver.PortNotFoundError) {
				continue
			}
			if err != nil {
				return "", err
			}
		}

		var resp *http.Response
		resp, err = http.Get("http://localhost:" + port)
		if err == nil {
			resp.Body.Close()
			return port, nil
		}

		time.Sleep(time.Millisecond * 100)
	}

	return "", xerrors.Errorf("failed while trying to find code-server port: %w", err)
}
