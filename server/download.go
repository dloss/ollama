package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jmorganca/ollama/api"
	"github.com/jmorganca/ollama/format"
)

var blobDownloadManager sync.Map

type blobDownload struct {
	Name   string
	Digest string

	Total     int64
	Completed atomic.Int64

	Parts []*blobDownloadPart

	context.CancelFunc

	done       bool
	err        error
	references atomic.Int32
}

type blobDownloadPart struct {
	N         int
	Offset    int64
	Size      int64
	Completed int64

	*blobDownload `json:"-"`
}

const (
	numDownloadParts          = 64
	minDownloadPartSize int64 = 32 * 1000 * 1000
	maxDownloadPartSize int64 = 256 * 1000 * 1000
)

func (p *blobDownloadPart) Name() string {
	return strings.Join([]string{
		p.blobDownload.Name, "partial", strconv.Itoa(p.N),
	}, "-")
}

func (p *blobDownloadPart) StartsAt() int64 {
	return p.Offset + p.Completed
}

func (p *blobDownloadPart) StopsAt() int64 {
	return p.Offset + p.Size
}

func (b *blobDownload) Prepare(ctx context.Context, requestURL *url.URL, opts *RegistryOptions) error {
	partFilePaths, err := filepath.Glob(b.Name + "-partial-*")
	if err != nil {
		return err
	}

	for _, partFilePath := range partFilePaths {
		part, err := b.readPart(partFilePath)
		if err != nil {
			return err
		}

		b.Total += part.Size
		b.Completed.Add(part.Completed)
		b.Parts = append(b.Parts, part)
	}

	if len(b.Parts) == 0 {
		resp, err := makeRequest(ctx, "HEAD", requestURL, nil, nil, opts)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode >= http.StatusBadRequest {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("registry responded with code %d: %v", resp.StatusCode, string(body))
		}

		b.Total, _ = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)

		var size = b.Total / numDownloadParts
		switch {
		case size < minDownloadPartSize:
			size = minDownloadPartSize
		case size > maxDownloadPartSize:
			size = maxDownloadPartSize
		}

		var offset int64
		for offset < b.Total {
			if offset+size > b.Total {
				size = b.Total - offset
			}

			if err := b.newPart(offset, size); err != nil {
				return err
			}

			offset += size
		}
	}

	log.Printf("downloading %s in %d %s part(s)", b.Digest[7:19], len(b.Parts), format.HumanBytes(b.Parts[0].Size))
	return nil
}

func (b *blobDownload) Run(ctx context.Context, requestURL *url.URL, opts *RegistryOptions) {
	b.err = b.run(ctx, requestURL, opts)
}

func (b *blobDownload) run(ctx context.Context, requestURL *url.URL, opts *RegistryOptions) error {
	defer blobDownloadManager.Delete(b.Digest)

	ctx, b.CancelFunc = context.WithCancel(ctx)

	file, err := os.OpenFile(b.Name+"-partial", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	file.Truncate(b.Total)

	g, inner := errgroup.WithContext(ctx)
	g.SetLimit(numDownloadParts)
	for i := range b.Parts {
		part := b.Parts[i]
		if part.Completed == part.Size {
			continue
		}

		i := i
		g.Go(func() error {
			for try := 0; try < maxRetries; try++ {
				w := io.NewOffsetWriter(file, part.StartsAt())
				err := b.downloadChunk(inner, requestURL, w, part, opts)
				switch {
				case errors.Is(err, context.Canceled), errors.Is(err, syscall.ENOSPC):
					// return immediately if the context is canceled or the device is out of space
					return err
				case err != nil:
					log.Printf("%s part %d attempt %d failed: %v, retrying", b.Digest[7:19], i, try, err)
					continue
				default:
					return nil
				}
			}

			return errors.New("max retries exceeded")
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	// explicitly close the file so we can rename it
	if err := file.Close(); err != nil {
		return err
	}

	for i := range b.Parts {
		if err := os.Remove(file.Name() + "-" + strconv.Itoa(i)); err != nil {
			return err
		}
	}

	if err := os.Rename(file.Name(), b.Name); err != nil {
		return err
	}

	b.done = true
	return nil
}

func (b *blobDownload) downloadChunk(ctx context.Context, requestURL *url.URL, w io.Writer, part *blobDownloadPart, opts *RegistryOptions) error {
	headers := make(http.Header)
	headers.Set("Range", fmt.Sprintf("bytes=%d-%d", part.StartsAt(), part.StopsAt()-1))
	resp, err := makeRequest(ctx, "GET", requestURL, headers, nil, opts)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	n, err := io.Copy(w, io.TeeReader(resp.Body, b))
	if err != nil && !errors.Is(err, context.Canceled) {
		// rollback progress
		b.Completed.Add(-n)
		return err
	}

	part.Completed += n
	if err := b.writePart(part.Name(), part); err != nil {
		return err
	}

	// return nil or context.Canceled
	return err
}

func (b *blobDownload) newPart(offset, size int64) error {
	part := blobDownloadPart{blobDownload: b, Offset: offset, Size: size, N: len(b.Parts)}
	if err := b.writePart(part.Name(), &part); err != nil {
		return err
	}

	b.Parts = append(b.Parts, &part)
	return nil
}

func (b *blobDownload) readPart(partName string) (*blobDownloadPart, error) {
	var part blobDownloadPart
	partFile, err := os.Open(partName)
	if err != nil {
		return nil, err
	}
	defer partFile.Close()

	if err := json.NewDecoder(partFile).Decode(&part); err != nil {
		return nil, err
	}

	part.blobDownload = b
	return &part, nil
}

func (b *blobDownload) writePart(partName string, part *blobDownloadPart) error {
	partFile, err := os.OpenFile(partName, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer partFile.Close()

	return json.NewEncoder(partFile).Encode(part)
}

func (b *blobDownload) Write(p []byte) (n int, err error) {
	n = len(p)
	b.Completed.Add(int64(n))
	return n, nil
}

func (b *blobDownload) acquire() {
	b.references.Add(1)
}

func (b *blobDownload) release() {
	if b.references.Add(-1) == 0 {
		b.CancelFunc()
	}
}

func (b *blobDownload) Wait(ctx context.Context, fn func(api.ProgressResponse)) error {
	b.acquire()
	defer b.release()

	ticker := time.NewTicker(60 * time.Millisecond)
	for {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}

		fn(api.ProgressResponse{
			Status:    fmt.Sprintf("downloading %s", b.Digest),
			Digest:    b.Digest,
			Total:     b.Total,
			Completed: b.Completed.Load(),
		})

		if b.done || b.err != nil {
			return b.err
		}
	}
}

type downloadOpts struct {
	mp      ModelPath
	digest  string
	regOpts *RegistryOptions
	fn      func(api.ProgressResponse)
}

const maxRetries = 3

// downloadBlob downloads a blob from the registry and stores it in the blobs directory
func downloadBlob(ctx context.Context, opts downloadOpts) error {
	fp, err := GetBlobsPath(opts.digest)
	if err != nil {
		return err
	}

	fi, err := os.Stat(fp)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		opts.fn(api.ProgressResponse{
			Status:    fmt.Sprintf("downloading %s", opts.digest),
			Digest:    opts.digest,
			Total:     fi.Size(),
			Completed: fi.Size(),
		})

		return nil
	}

	data, ok := blobDownloadManager.LoadOrStore(opts.digest, &blobDownload{Name: fp, Digest: opts.digest})
	download := data.(*blobDownload)
	if !ok {
		requestURL := opts.mp.BaseURL()
		requestURL = requestURL.JoinPath("v2", opts.mp.GetNamespaceRepository(), "blobs", opts.digest)
		if err := download.Prepare(ctx, requestURL, opts.regOpts); err != nil {
			blobDownloadManager.Delete(opts.digest)
			return err
		}

		go download.Run(context.Background(), requestURL, opts.regOpts)
	}

	return download.Wait(ctx, opts.fn)
}
