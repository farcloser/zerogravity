// Package download reads one large file over HTTP and survives the line
// dropping: a body cut before its end is resumed in place with a Range request,
// so the consumer sees one uninterrupted stream and never a partial file.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mycophonic/primordium/network/transporter"
)

var (
	// ErrDownload reports a transfer that did not complete.
	ErrDownload = errors.New("download")
	// ErrChanged reports a resource that changed between two requests: the bytes
	// already delivered belong to another publication than the rest would.
	ErrChanged = errors.New("the file changed on the server mid-transfer")
)

const (
	// attempts is how many consecutive cuts with no byte in between end the
	// transfer; a cut after progress starts the count over. With the backoff
	// below that is four minutes of a dead line, which no test waits for:
	// giving up and the reset on progress are held by reading, not by a test.
	attempts       = 8
	initialBackoff = 2 * time.Second
	maxBackoff     = time.Minute
	progressEvery  = 2 * time.Second
	userAgent      = "zerogravity"
	// unknownSize is the total when the server did not say.
	unknownSize = int64(-1)
)

// Progress receives the bytes delivered so far and the total, or -1 when the
// server did not say. It is called at most every couple of seconds, and once
// at the end.
type Progress func(done, total int64)

// Open starts fetching url and returns the body as one stream. A body the
// server or the network cuts short is resumed from the byte reached, with a
// backoff between attempts, and the resumed bytes are held to the first
// response's ETag or Last-Modified through If-Range: a file republished
// mid-transfer is an error, never a splice. The stream ends with io.EOF only
// at the size the first response announced.
func Open(ctx context.Context, url string, progress Progress) (io.ReadCloser, error) {
	reader := &reader{
		ctx: ctx,
		url: url,
		client: transporter.NewClient(transporter.Options{
			MaxRetries:     3,
			InitialBackoff: initialBackoff,
			MaxBackoff:     maxBackoff,
			UserAgent:      userAgent,
		}),
		progress: progress,
		total:    unknownSize,
	}

	response, err := reader.request()
	if err != nil {
		return nil, err
	}

	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()

		return nil, fmt.Errorf("%w: %s: %s", ErrDownload, url, response.Status)
	}

	reader.body = response.Body
	reader.total = response.ContentLength
	reader.validator = validator(response.Header)

	reader.report(true)

	return reader, nil
}

// validator is what If-Range may carry: a strong ETag, else Last-Modified. A
// weak ETag (W/"...") is forbidden there (RFC 9110 §13.1.5) and a server
// seeing one answers 200, which would read as the file having changed.
func validator(header http.Header) string {
	if etag := header.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		return etag
	}

	return header.Get("Last-Modified")
}

type reader struct {
	ctx      context.Context //nolint:containedctx // the stream outlives Open; its reads are bounded by the caller's context
	url      string
	client   *http.Client
	progress Progress

	body      io.ReadCloser
	done      int64
	total     int64
	validator string

	failures int
	backoff  time.Duration
	reported time.Time
	closed   bool
}

// Read delivers the next bytes, resuming across cuts; it returns 0, nil never.
func (r *reader) Read(buffer []byte) (int, error) {
	if r.closed {
		return 0, fmt.Errorf("%w: read after close", ErrDownload)
	}

	for {
		body := r.body
		if body == nil {
			resumed, err := r.resume()
			if err != nil {
				return 0, err
			}

			body = resumed
			r.body = resumed
		}

		read, err := body.Read(buffer)
		r.done += int64(read)

		if read > 0 {
			r.failures = 0
			r.report(false)
		}

		switch {
		case err == nil:
			return read, nil
		case errors.Is(err, io.EOF) && (r.total == unknownSize || r.done == r.total):
			r.report(true)

			return read, io.EOF
		case r.ctx.Err() != nil:
			return read, fmt.Errorf("%w: %w", ErrDownload, r.ctx.Err())
		}

		// Cut: io.EOF before the size, io.ErrUnexpectedEOF, a reset. The next
		// turn of the loop resumes, after a pause.
		_ = body.Close()
		r.body = nil

		if read > 0 {
			return read, nil
		}
	}
}

// Close ends the stream; nothing is resumed after it.
func (r *reader) Close() error {
	defer r.client.CloseIdleConnections()

	r.closed = true

	if r.body == nil {
		return nil
	}

	if err := r.body.Close(); err != nil {
		return fmt.Errorf("%w: %w", ErrDownload, err)
	}

	return nil
}

// resume waits out the backoff and asks for the bytes from done on, returning
// the body that continues the stream.
func (r *reader) resume() (io.ReadCloser, error) {
	r.failures++
	if r.failures > attempts {
		return nil, fmt.Errorf("%w: gave up after %d consecutive cuts at byte %d", ErrDownload, attempts, r.done)
	}

	if r.backoff == 0 {
		r.backoff = initialBackoff
	}

	select {
	case <-r.ctx.Done():
		return nil, fmt.Errorf("%w: %w", ErrDownload, r.ctx.Err())
	case <-time.After(r.backoff):
	}

	r.backoff = min(r.backoff*2, maxBackoff)

	response, err := r.request()
	if err != nil {
		// A request that fails outright counts as a cut and is retried the
		// same way; the caller's context is the only way out.
		if r.ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", ErrDownload, r.ctx.Err())
		}

		return r.resume()
	}

	if err = r.check(response); err != nil {
		_ = response.Body.Close()

		return nil, err
	}

	r.backoff = initialBackoff

	return response.Body, nil
}

// check holds a resumed response to the bytes already delivered.
func (r *reader) check(response *http.Response) error {
	switch response.StatusCode {
	case http.StatusPartialContent:
		start, total, err := parseContentRange(response.Header.Get("Content-Range"))
		if err != nil {
			return err
		}

		if start != r.done || (r.total != unknownSize && total != r.total) {
			return fmt.Errorf(
				"%w: asked for bytes %d- of %d, got %d- of %d", ErrDownload, r.done, r.total, start, total,
			)
		}

		// A first response without a size is made up for by the range: from
		// here the stream ends at total, not at whatever EOF comes.
		r.total = total

		return nil
	case http.StatusOK:
		// The whole file again: the server either ignores ranges or, through
		// If-Range, says the file is not the one we started on.
		return fmt.Errorf("%w: %w", ErrDownload, ErrChanged)
	default:
		return fmt.Errorf("%w: resuming at byte %d: %s", ErrDownload, r.done, response.Status)
	}
}

// request issues the GET: whole when nothing was delivered yet, from done on
// otherwise, held to the first response's validator when it gave one.
func (r *reader) request() (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDownload, err)
	}

	if r.done > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(r.done, 10)+"-")

		if r.validator != "" {
			req.Header.Set("If-Range", r.validator)
		}
	}

	response, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDownload, err)
	}

	return response, nil
}

// report calls progress, at most every progressEvery unless forced.
func (r *reader) report(force bool) {
	if r.progress == nil {
		return
	}

	now := time.Now()
	if !force && now.Sub(r.reported) < progressEvery {
		return
	}

	r.reported = now
	r.progress(r.done, r.total)
}

// parseContentRange reads "bytes <first>-<last>/<size>" into first and size.
func parseContentRange(header string) (first, size int64, err error) {
	rest, found := strings.CutPrefix(header, "bytes ")
	if !found {
		return 0, 0, fmt.Errorf("%w: Content-Range %q", ErrDownload, header)
	}

	span, sizeText, found := strings.Cut(rest, "/")
	if !found {
		return 0, 0, fmt.Errorf("%w: Content-Range %q", ErrDownload, header)
	}

	firstText, _, _ := strings.Cut(span, "-")

	first, err = strconv.ParseInt(firstText, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: Content-Range %q: %w", ErrDownload, header, err)
	}

	size, err = strconv.ParseInt(sizeText, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: Content-Range %q: %w", ErrDownload, header, err)
	}

	return first, size, nil
}
