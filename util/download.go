package util

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"govd/models"
	"govd/util/av"

	"github.com/google/uuid"
)

var downloadHTTPSession = GetDefaultHTTPClient()

func DefaultConfig() *models.DownloadConfig {
	downloadsDir := os.Getenv("DOWNLOADS_DIR")
	if downloadsDir == "" {
		downloadsDir = "downloads"
	}
	return &models.DownloadConfig{
		ChunkSize:     10 * 1024 * 1024, // 10MB
		Concurrency:   4,
		Timeout:       30 * time.Second,
		DownloadDir:   downloadsDir,
		RetryAttempts: 3,
		RetryDelay:    2 * time.Second,
		Remux:         true,
		MaxInMemory:   50 * 1024 * 1024, // 50MB
	}
}

func DownloadFile(
	ctx context.Context,
	URLList []string,
	fileName string,
	config *models.DownloadConfig,
) (string, error) {
	if config == nil {
		config = DefaultConfig()
	}

	var errs []error
	for _, fileURL := range URLList {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
			// create the download directory if it doesn't exist
			if err := EnsureDownloadDir(config.DownloadDir); err != nil {
				return "", err
			}

			filePath := filepath.Join(config.DownloadDir, fileName)
			err := runChunkedDownload(ctx, fileURL, filePath, config)
			if err != nil {
				errs = append(errs, err)
				continue
			}

			if config.Remux {
				err := av.RemuxFile(filePath)
				if err != nil {
					os.Remove(filePath)
					return "", fmt.Errorf("remuxing failed: %w", err)
				}
			}
			return filePath, nil
		}
	}

	return "", fmt.Errorf("%w: %v", ErrDownloadFailed, errs)
}

func DownloadFileWithSegments(
	ctx context.Context,
	segmentURLs []string,
	fileName string,
	config *models.DownloadConfig,
) (string, error) {
	if config == nil {
		config = DefaultConfig()
	}
	if err := EnsureDownloadDir(config.DownloadDir); err != nil {
		return "", err
	}
	tempDir := filepath.Join(
		config.DownloadDir,
		"segments"+uuid.NewString(),
	)
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create temporary directory: %w", err)
	}

	downloadedFiles, err := downloadSegments(ctx, tempDir, segmentURLs, config)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("failed to download segments: %w", err)
	}
	mergedFilePath, err := av.MergeSegments(downloadedFiles, fileName)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", fmt.Errorf("failed to merge segments: %w", err)
	}
	if err := os.RemoveAll(tempDir); err != nil {
		return "", fmt.Errorf("failed to remove temporary directory: %w", err)
	}
	return mergedFilePath, nil
}

func DownloadFileInMemory(
	ctx context.Context,
	URLList []string,
	config *models.DownloadConfig,
) (*bytes.Reader, error) {
	if config == nil {
		config = DefaultConfig()
	}

	var errs []error
	for _, fileURL := range URLList {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			data, err := downloadInMemory(
				ctx, fileURL,
				config,
			)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			return bytes.NewReader(data), nil
		}
	}

	return nil, fmt.Errorf("%w: %v", ErrDownloadFailed, errs)
}

func downloadInMemory(
	ctx context.Context,
	fileURL string,
	config *models.DownloadConfig,
) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(
		ctx,
		config.Timeout,
	)
	defer cancel()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		// continue with the request
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := downloadHTTPSession.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	if resp.ContentLength > int64(config.MaxInMemory) {
		return nil, fmt.Errorf("file too large for in-memory download: %d bytes", resp.ContentLength)
	}

	var bufPool = sync.Pool{
		New: func() any {
			return bytes.NewBuffer(make([]byte, 0, 1024*1024))
		},
	}

	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	}

	_, err = io.Copy(buf, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return buf.Bytes(), nil
}

func EnsureDownloadDir(dir string) error {
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create downloads directory: %w", err)
			}
		} else {
			return fmt.Errorf("error accessing directory: %w", err)
		}
	}
	return nil
}

func runChunkedDownload(
	ctx context.Context,
	fileURL string,
	filePath string,
	config *models.DownloadConfig,
) error {
	// reduce concurrency if it's greater
	// than the number of available CPUs
	maxProcs := runtime.GOMAXPROCS(0)
	optimalConcurrency := int(math.Max(1, float64(maxProcs-1)))

	if config.Concurrency > optimalConcurrency {
		config.Concurrency = optimalConcurrency
	}

	fileSize, err := getFileSize(ctx, fileURL, config.Timeout)
	if err != nil {
		return err
	}

	file, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	// pre-allocate file size if possible
	if fileSize > 0 {
		if err := file.Truncate(int64(fileSize)); err != nil {
			return fmt.Errorf("failed to allocate file space: %w", err)
		}
	}

	chunks := createChunks(fileSize, config.ChunkSize)

	semaphore := make(chan struct{}, config.Concurrency)
	var wg sync.WaitGroup

	errChan := make(chan error, len(chunks))
	var downloadErr error
	var errOnce sync.Once

	var completedChunks atomic.Int64
	var completedBytes atomic.Int64

	downloadCtx, cancelDownload := context.WithCancel(ctx)
	defer cancelDownload()

	for idx, chunk := range chunks {
		wg.Add(1)

		go func(idx int, chunk [2]int) {
			defer wg.Done()

			// respect concurrency limit
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-downloadCtx.Done():
				return
			}

			chunkData, err := downloadChunkWithRetry(downloadCtx, fileURL, chunk, config)
			if err != nil {
				errOnce.Do(func() {
					downloadErr = fmt.Errorf("chunk %d: %w", idx, err)
					cancelDownload() // cancel all other downloads
					errChan <- downloadErr
				})
				return
			}

			if err := writeChunkToFile(file, chunkData, chunk[0]); err != nil {
				errOnce.Do(func() {
					downloadErr = fmt.Errorf("failed to write chunk %d: %w", idx, err)
					cancelDownload()
					errChan <- downloadErr
				})
				return
			}

			// update progress
			chunkSize := chunk[1] - chunk[0] + 1
			completedChunks.Add(1)
			completedBytes.Add(int64(chunkSize))
			progress := float64(completedBytes.Load()) / float64(fileSize)

			// report progress if handler exists
			if config.ProgressUpdater != nil {
				config.ProgressUpdater(progress)
			}
		}(idx, chunk)
	}

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(errChan)
		close(done)
	}()

	var multiErr []error

	select {
	case err := <-errChan:
		if err != nil {
			multiErr = append(multiErr, err)
			// collect all errors
			for e := range errChan {
				if e != nil {
					multiErr = append(multiErr, e)
				}
			}
		}
		<-done
	case <-ctx.Done():
		cancelDownload()
		<-done // wait for all goroutines to finish
		os.Remove(filePath)
		return ctx.Err()
	case <-done:
		// no errors
	}

	if len(multiErr) > 0 {
		os.Remove(filePath)
		return fmt.Errorf("multiple download errors: %v", multiErr)
	}

	return nil
}

func getFileSize(ctx context.Context, fileURL string, timeout time.Duration) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodHead, fileURL, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := downloadHTTPSession.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to get file size: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("failed to get file info: status code %d", resp.StatusCode)
	}

	return int(resp.ContentLength), nil
}

func downloadChunkWithRetry(
	ctx context.Context,
	fileURL string,
	chunk [2]int,
	config *models.DownloadConfig,
) ([]byte, error) {
	var lastErr error

	for attempt := 0; attempt <= config.RetryAttempts; attempt++ {
		if attempt > 0 {
			// wait before retry
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(config.RetryDelay):
			}
		}

		data, err := downloadChunk(ctx, fileURL, chunk, config.Timeout)
		if err == nil {
			return data, nil
		}

		lastErr = err
	}

	return nil, fmt.Errorf("all %d attempts failed: %w", config.RetryAttempts+1, lastErr)
}

func downloadFile(
	ctx context.Context,
	fileURL string,
	filePath string,
	timeout time.Duration,
) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fileURL, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	resp, err := downloadHTTPSession.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	file, err := os.Create(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()
	_, err = io.Copy(file, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}
	return filePath, nil
}

func downloadChunk(
	ctx context.Context,
	fileURL string,
	chunk [2]int,
	timeout time.Duration,
) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d", chunk[0], chunk[1]))

	resp, err := downloadHTTPSession.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var buf bytes.Buffer
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	} else {
		buf.Grow(chunk[1] - chunk[0] + 1)
	}
	_, err = io.Copy(&buf, resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read chunk data: %w", err)
	}

	return buf.Bytes(), nil
}

func writeChunkToFile(file *os.File, data []byte, offset int) error {
	_, err := file.WriteAt(data, int64(offset))
	return err
}

func createChunks(fileSize int, chunkSize int) [][2]int {
	if fileSize <= 0 {
		return [][2]int{{0, 0}}
	}

	numChunks := int(math.Ceil(float64(fileSize) / float64(chunkSize)))
	chunks := make([][2]int, numChunks)

	for i := range chunks {
		start := i * chunkSize
		end := start + chunkSize - 1
		if end >= fileSize {
			end = fileSize - 1
		}
		chunks[i] = [2]int{start, end}
	}

	return chunks
}

func downloadSegments(
	ctx context.Context,
	path string,
	segmentURLs []string,
	config *models.DownloadConfig,
) ([]string, error) {
	if config == nil {
		config = DefaultConfig()
	}

	semaphore := make(chan struct{}, config.Concurrency)
	var wg sync.WaitGroup

	var firstErr atomic.Value

	downloadedFiles := make([]string, len(segmentURLs))
	defer func() {
		if firstErr.Load() != nil {
			for _, path := range downloadedFiles {
				if path != "" {
					os.Remove(path)
				}
			}
		}
	}()

	downloadCtx, cancelDownload := context.WithCancel(ctx)
	defer cancelDownload()

	for i, segmentURL := range segmentURLs {
		wg.Add(1)
		go func(idx int, url string) {
			defer wg.Done()

			select {
			case <-downloadCtx.Done():
				return
			default:
				// continue with the download
			}

			// acquire semaphore slot
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			segmentFileName := fmt.Sprintf("segment_%05d", idx)
			segmentPath := filepath.Join(path, segmentFileName)

			filePath, err := downloadFile(
				ctx, url, segmentPath,
				config.Timeout,
			)

			if err != nil {
				if firstErr.Load() == nil {
					firstErr.Store(fmt.Errorf("failed to download segment %d: %w", idx, err))
					cancelDownload()
				}
				return
			}

			downloadedFiles[idx] = filePath
		}(i, segmentURL)
	}
	wg.Wait()

	if err := firstErr.Load(); err != nil {
		return nil, err.(error)
	}

	for i, file := range downloadedFiles {
		if file == "" {
			return nil, fmt.Errorf("segment %d was not downloaded", i)
		}
		if _, err := os.Stat(file); os.IsNotExist(err) {
			return nil, fmt.Errorf("segment %d file does not exist: %w", i, err)
		}
	}

	return downloadedFiles, nil
}
