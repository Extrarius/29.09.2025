package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Скачивание файла по URL с ретраями и атомарным rename.
type Options struct {
	ClientTimeout   time.Duration
	Retries         int
	HostConcurrency int
}

type Downloader struct {
	httpClient *http.Client
	opts       Options
	hostSem    map[string]chan struct{}
}

// NewDownloader создаёт загрузчик с переданными опциями.
//
// Инициализирует:
//   - httpClient с таймаутом opts.ClientTimeout;
//   - карту семафоров hostSem для ограничения параллелизма по хостам
//     (используется вместе с opts.HostConcurrency);
//   - сохраняет opts (включая Retries и др.).
func NewDownloader(opts Options) *Downloader {
	return &Downloader{
		httpClient: &http.Client{Timeout: opts.ClientTimeout},
		opts:       opts,
		hostSem:    make(map[string]chan struct{}),
	}
}

// acquireHost захватывает слот параллелизма для указанного хоста
// и возвращает release-функцию, которую нужно вызвать (обычно defer release())
// для освобождения слота.
//
// Логика:
//   - если HostConcurrency <= 0 — ограничение отключено (возвращается no-op release);
//   - для каждого host лениво создаётся буферизированный канал-семафор
//     ёмкостью HostConcurrency;
//   - запись в канал блокирует при исчерпании слотов, тем самым ограничивая
//     одновременные загрузки с этого хоста;
//   - release() читает из канала, освобождая слот.
//
// ВАЖНО: d.hostSem — обычная map; её заполнение при конкурентном доступе
// нужно синхронизировать (мьютексом или sync.Map), иначе возможны гонки / panic
// «concurrent map writes».
func (d *Downloader) acquireHost(host string) func() {
	if d.opts.HostConcurrency <= 0 {
		return func() {}
	}
	sem, ok := d.hostSem[host]
	if !ok {
		sem = make(chan struct{}, d.opts.HostConcurrency)
		d.hostSem[host] = sem
	}
	sem <- struct{}{}
	return func() { <-sem }
}

// Fetch скачивает ресурс по rawURL в файл destPath.
//
// Поведение:
//   - ограничивает параллелизм по хосту (acquireHost/release);
//   - делает до max(1, d.opts.Retries) попыток с экспоненциальным backoff;
//   - пишет во временный файл destPath+".part" и по успеху атомарно переименовывает;
//   - создаёт директорию назначения при необходимости;
//   - прерывается по ctx (таймаут/отмена).
//
// Возвращает количество записанных байт или ошибку.
// Примечания: 5xx ⇒ ретрай; 4xx ⇒ немедленная ошибка; временные файлы удаляются на ошибках.
func (d *Downloader) Fetch(ctx context.Context, rawURL, destPath string) (int64, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	release := d.acquireHost(u.Host)
	defer release()

	var lastErr error
	backoff := 500 * time.Millisecond

	for attempt := 0; attempt < max(1, d.opts.Retries); attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return 0, err
		}

		tmpPath := destPath + ".part"
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return 0, err
		}
		out, err := os.Create(tmpPath)
		if err != nil {
			return 0, err
		}

		resp, err := d.httpClient.Do(req)
		if err != nil {
			out.Close()
			lastErr = err
			select {
			case <-time.After(backoff):
				backoff *= 2
				continue
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		if resp.Body != nil {
			defer resp.Body.Close()
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			io.Copy(io.Discard, resp.Body)
			out.Close()
			os.Remove(tmpPath)
			lastErr = fmt.Errorf("http %d", resp.StatusCode)
			if resp.StatusCode >= 500 && resp.StatusCode < 600 {
				select {
				case <-time.After(backoff):
					backoff *= 2
					continue
				case <-ctx.Done():
					return 0, ctx.Err()
				}
			}
			return 0, lastErr
		}

		if cl := resp.Header.Get("Content-Length"); cl != "" {
			if _, err := strconv.ParseInt(cl, 10, 64); err == nil {
				// можно логировать/передавать как SizeHint
			}
		}

		written, copyErr := io.Copy(out, resp.Body)
		closeErr := out.Close()
		if copyErr != nil {
			lastErr = copyErr
			os.Remove(tmpPath)
			select {
			case <-time.After(backoff):
				backoff *= 2
				continue
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		if closeErr != nil {
			lastErr = closeErr
			os.Remove(tmpPath)
			select {
			case <-time.After(backoff):
				backoff *= 2
				continue
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}

		if err := os.Rename(tmpPath, destPath); err != nil {
			lastErr = err
			os.Remove(tmpPath)
			select {
			case <-time.After(backoff):
				backoff *= 2
				continue
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		return written, nil
	}
	if lastErr == nil {
		lastErr = errors.New("неизвестная ошибка при скачивании")
	}
	return 0, lastErr
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
