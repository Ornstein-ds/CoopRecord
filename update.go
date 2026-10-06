package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const appVersion = "1.0.1"
const releaseAPI = "https://api.github.com/repos/Ornstein-ds/CoopRecord/releases/latest"
const updateArchive = "CoopRecord-windows-x64.zip"
const maxUpdateSize = 64 << 20

type githubRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Digest string `json:"digest"`
		Size   int64  `json:"size"`
	} `json:"assets"`
}

func parseVersion(s string) ([3]int, error) {
	var result [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != len(result) {
		return result, fmt.Errorf("неизвестный формат версии: %q", s)
	}
	for i, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return result, fmt.Errorf("неизвестный формат версии: %q", s)
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return result, fmt.Errorf("неизвестный формат версии: %q", s)
		}
		result[i] = n
	}
	return result, nil
}

func updateGET(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "CoopRecord/"+appVersion)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("не удалось связаться с GitHub: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		switch resp.StatusCode {
		case 404:
			return nil, fmt.Errorf("релиз недоступен: проверьте, что репозиторий публичный и содержит опубликованный релиз")
		case 403, 429:
			return nil, fmt.Errorf("GitHub ограничил запросы. Попробуйте позже")
		default:
			return nil, fmt.Errorf("GitHub вернул HTTP %d", resp.StatusCode)
		}
	}
	return resp, nil
}

// Returns an empty path when the installed version is current or newer.
func downloadUpdate(ctx context.Context, client *http.Client, api, current, directory string) (path, tag string, err error) {
	resp, err := updateGET(ctx, client, api)
	if err != nil {
		return "", "", err
	}
	var release githubRelease
	err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release)
	resp.Body.Close()
	if err != nil {
		return "", "", fmt.Errorf("не удалось прочитать описание релиза: %w", err)
	}
	latest, err := parseVersion(release.Tag)
	if err != nil || release.Draft || release.Prerelease {
		return "", "", fmt.Errorf("GitHub не вернул стабильный релиз с версией vX.Y.Z")
	}
	installed, err := parseVersion(current)
	if err != nil {
		return "", "", err
	}
	newer := false
	for i := range latest {
		if latest[i] != installed[i] {
			newer = latest[i] > installed[i]
			break
		}
	}
	if !newer {
		return "", release.Tag, nil
	}
	for _, asset := range release.Assets {
		if asset.Name != updateArchive {
			continue
		}
		if !strings.HasPrefix(asset.URL, "https://github.com/Ornstein-ds/CoopRecord/releases/download/") {
			return "", "", fmt.Errorf("неожиданный адрес архива обновления")
		}
		expected, err := hex.DecodeString(strings.TrimPrefix(asset.Digest, "sha256:"))
		if !strings.HasPrefix(asset.Digest, "sha256:") || err != nil || len(expected) != sha256.Size || asset.Size <= 0 || asset.Size > maxUpdateSize {
			return "", "", fmt.Errorf("у архива нет корректной SHA-256 или недопустимый размер")
		}
		resp, err := updateGET(ctx, client, asset.URL)
		if err != nil {
			return "", "", err
		}
		defer resp.Body.Close()
		dir := filepath.Join(directory, fmt.Sprintf("v%d.%d.%d", latest[0], latest[1], latest[2]))
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", "", err
		}
		file, err := os.CreateTemp(dir, "update-*.partial")
		if err != nil {
			return "", "", err
		}
		defer os.Remove(file.Name())
		hash := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, asset.Size+1))
		closeErr := file.Close()
		if copyErr != nil {
			return "", "", fmt.Errorf("загрузка прервалась: %w", copyErr)
		}
		if closeErr != nil {
			return "", "", closeErr
		}
		if n != asset.Size || !bytes.Equal(hash.Sum(nil), expected) {
			return "", "", fmt.Errorf("архив обновления повреждён: размер или SHA-256 не совпадает. Повторите загрузку")
		}
		path = filepath.Join(dir, updateArchive)
		if err := os.Rename(file.Name(), path); err != nil {
			return "", "", err
		}
		return path, release.Tag, nil
	}
	return "", "", fmt.Errorf("в релизе %s нет %s", release.Tag, updateArchive)
}

func checkAppUpdate() (string, string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || len(via) >= 10 {
			return fmt.Errorf("недопустимое перенаправление при загрузке обновления")
		}
		return nil
	}}
	return downloadUpdate(ctx, client, releaseAPI, appVersion, filepath.Join(dir, "CoopRecord", "Updates"))
}
