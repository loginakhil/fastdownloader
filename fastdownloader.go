package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrNoParallelDownload = errors.New("parallel download not supported")

const (
	contentLengthHeader      = "Content-Length"
	contentDispositionHeader = "Content-Disposition"
)

func downloadRangeBytes(
	ctx context.Context,
	fileName string,
	progress io.Writer,
	start, stop uint64,
	url string,
) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, stop))

	res, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return err
	}

	defer func() { _ = res.Body.Close() }()

	dataWriter(fileName, res.Body, progress)

	return nil
}

func parseURLAndCaptureFilename(downloadURL string) (string, error) {
	u, err := url.Parse(downloadURL)
	if err != nil {
		return "", err
	}

	return path.Base(u.Path), nil
}

func extractDownloadDetailsFromHeaders(header http.Header) (
	filename string,
	fileLength uint64,
	err error,
) {
	fileLength, err = strconv.ParseUint(header.Get(contentLengthHeader), 10, 64)
	if err != nil {
		return
	}

	contentDisposition := header.Get(contentDispositionHeader)
	if len(contentDisposition) == 0 {
		return
	}

	_, params, err := mime.ParseMediaType(contentDisposition)
	if err != nil {
		return
	}

	filename = params["filename"]

	return
}

func getHeaders(ctx context.Context, url string) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, fmt.Errorf("http.head request creation failed %w", err)
	}

	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("http.head request failed %w", err)
	}

	_ = res.Body.Close()

	return res.Header, nil
}

func formatBytes(num float64, suffix string) string {
	const byteSize = 1024.0

	for _, unit := range []string{"", "Ki", "Mi", "Gi", "Ti", "Pi", "Ei", "Zi"} {
		if math.Abs(num) < byteSize {
			return fmt.Sprintf("%3.1f %s%s", num, unit, suffix)
		}

		num /= byteSize
	}

	return fmt.Sprintf("%.1f %s%s", num, "Yi", suffix)
}

type progressWriter struct {
	maxBytes  uint64
	readBytes uint64
}

func (p *progressWriter) Write(data []byte) (n int, err error) {
	const maxColumns = 80

	p.readBytes += uint64(len(data))

	fmt.Printf("\r%s", strings.Repeat(" ", maxColumns))
	fmt.Printf(
		"\rProgress [%s/%s] (%d%%)",
		formatBytes(float64(p.readBytes), ""),
		formatBytes(float64(p.maxBytes), ""),
		int(math.Ceil(float64(p.readBytes)/float64(p.maxBytes)*100.0)), //nolint:gomnd
	)

	return len(data), nil
}

func batchGenerator(contentLength, totalBatches uint64) func() (uint64, uint64) {
	var (
		m         sync.Mutex
		start     = uint64(0)
		batchSize = contentLength / totalBatches
	)

	return func() (uint64, uint64) {
		m.Lock()
		defer m.Unlock()

		if start >= contentLength {
			return uint64(0), uint64(0)
		}

		stop := start + batchSize
		start += batchSize

		if stop > contentLength {
			stop = contentLength
		}

		return start - batchSize, stop - 1
	}
}

func serialDownload(ctx context.Context, downloadURL string) (string, error) {
	fallbackFileName, err := parseURLAndCaptureFilename(downloadURL)
	if err != nil {
		return "", err
	}

	if fallbackFileName == "" {
		fallbackFileName = "index.html"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}

	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return "", err
	}

	defer func() { _ = res.Body.Close() }()

	fileName, contentLength, err := extractDownloadDetailsFromHeaders(res.Header)
	if err != nil {
		return "", err
	}

	if fileName == "" {
		fileName = fallbackFileName
	}

	progress := &progressWriter{
		maxBytes: contentLength,
	}

	dataWriter(fileName, res.Body, progress)

	return fileName, nil
}

func dataWriter(
	fileName string,
	dataReader io.Reader,
	progressWriter io.Writer,
) {
	file, err := os.Create(fileName)
	if err != nil {
		panic(err)
	}

	defer func() { _ = file.Close() }()

	_, err = io.Copy(io.MultiWriter(file, progressWriter), dataReader)
	if err != nil {
		panic(err)
	}
}

func parallelDownload(ctx context.Context, downloadURL string, parallelRequests uint64) (string, error) {
	fallbackFileName, err := parseURLAndCaptureFilename(downloadURL)
	if err != nil {
		return "", err
	}

	headers, err := getHeaders(ctx, downloadURL)
	if err != nil {
		return "", err
	}

	if "bytes" != headers.Get("Accept-Ranges") {
		return "", ErrNoParallelDownload
	}

	fileName, contentLength, err := extractDownloadDetailsFromHeaders(headers)
	if err != nil {
		return "", err
	}

	if fileName == "" {
		fileName = fallbackFileName
	}

	var (
		downloaderWg sync.WaitGroup
	)

	progress := &progressWriter{
		maxBytes: contentLength,
	}

	generator := batchGenerator(contentLength, parallelRequests)

	var maxFiles int
	for {
		startRange, stopRange := generator()
		if startRange == 0 && stopRange == 0 {
			break
		}

		downloaderWg.Add(1)

		go func(index int, start, stop uint64) {
			defer downloaderWg.Done()

			err := downloadRangeBytes(
				ctx,
				fmt.Sprintf("%s.%d", fileName, index),
				progress,
				start,
				stop,
				downloadURL,
			)
			if err != nil {
				panic(err)
			}
		}(maxFiles, startRange, stopRange)

		maxFiles++
	}

	downloaderWg.Wait()

	finalFileName := fmt.Sprintf("%s.0", fileName)
	targetFile, err := os.OpenFile(finalFileName, os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		panic(err)
	}

	for i := 1; i < maxFiles; i++ {
		currentFileName := fmt.Sprintf("%s.%d", fileName, i)
		dataFile, err := os.Open(currentFileName)
		if err != nil {
			panic(err)
		}

		_, _ = io.Copy(targetFile, dataFile)

		_ = dataFile.Close()
		_ = os.Remove(currentFileName)
	}
	_ = targetFile.Close()

	_ = os.Rename(finalFileName, fileName)

	return fileName, nil
}

func main() {
	var (
		exitCode                int
		downloadURL             string
		parallelConnections     uint64
		defaultParallelRequests uint64 = 5
	)

	flag.StringVar(&downloadURL, "url", "", "provide the download URL")
	flag.Uint64Var(&parallelConnections, "parallel", defaultParallelRequests, "parallel requests")

	flag.Parse()

	if downloadURL == "" {
		flag.PrintDefaults()

		return
	}

	startTime := time.Now()
	ctx, cancelFN := context.WithCancel(context.Background())

	defer func() {
		cancelFN()
		os.Exit(exitCode)
	}()

	fileName, err := parallelDownload(ctx, downloadURL, parallelConnections)
	if errors.Is(err, ErrNoParallelDownload) {
		fmt.Println("Parallel download not supported, falling back to normal download")

		fileName, err = serialDownload(ctx, downloadURL)
	}

	fmt.Println()

	if err != nil {
		fmt.Printf("Download failed with error (%s) \n", err.Error())

		exitCode = -1

		return
	}

	fmt.Printf("Downloaded filename: %s \n", fileName)
	fmt.Printf("Total time: %d seconds \n", uint64(time.Since(startTime).Seconds()))
}
