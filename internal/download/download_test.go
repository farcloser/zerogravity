package download_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/farcloser/zerogravity/internal/download"
)

const (
	etag         = `"v1"`
	lastModified = "Mon, 19 Oct 2026 00:00:00 GMT"
	maxRequests  = 6
)

// cutting serves payload with Range support and closes the connection after
// cutAfter bytes of the first response. It records every request's Range and
// If-Range and the bytes it served. With changed set it answers a ranged
// request with the whole file, as a server does when If-Range does not match;
// with sizeless set the first response carries no Content-Length.
type cutting struct {
	payload  []byte
	cutAfter int
	changed  bool
	sizeless bool
	// shortResume makes the second response chunked and ends it cleanly after
	// that many bytes, below what its Content-Range promised.
	shortResume int
	// misranged answers every ranged request with the whole file's range,
	// "bytes 0-", whatever was asked; missized names another total.
	misranged bool
	missized  bool
	// weak hands out a weak ETag, which If-Range must not carry.
	weak bool

	mu       sync.Mutex
	ranges   []string
	ifRanges []string
	served   int
}

func (c *cutting) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	c.mu.Lock()
	c.ranges = append(c.ranges, request.Header.Get("Range"))
	c.ifRanges = append(c.ifRanges, request.Header.Get("If-Range"))
	first := len(c.ranges) == 1
	second := len(c.ranges) == 2
	c.mu.Unlock()

	// A client that never stops asking hangs the test; a cap turns that into
	// a failure.
	if len(c.ranges) > maxRequests {
		http.Error(writer, "too many requests for one test", http.StatusTeapot)

		return
	}

	start := 0

	if c.weak {
		writer.Header().Set("ETag", "W/"+etag)
		writer.Header().Set("Last-Modified", lastModified)
	} else {
		writer.Header().Set("ETag", etag)
	}

	writer.Header().Set("Accept-Ranges", "bytes")

	switch {
	case request.Header.Get("Range") != "" && !c.changed:
		value, _ := strings.CutPrefix(request.Header.Get("Range"), "bytes=")
		firstByte, _, _ := strings.Cut(value, "-")
		start, _ = strconv.Atoi(firstByte)

		if c.misranged {
			start = 0
		}

		total := len(c.payload)
		if c.missized {
			total++
		}

		writer.Header().Set(
			"Content-Range",
			"bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(total-1)+"/"+strconv.Itoa(total),
		)

		if !(second && c.shortResume > 0) {
			writer.Header().Set("Content-Length", strconv.Itoa(len(c.payload)-start))
		}

		writer.WriteHeader(http.StatusPartialContent)
	case c.sizeless:
		// Chunked: the client learns the size from nothing but the end.
	default:
		writer.Header().Set("Content-Length", strconv.Itoa(len(c.payload)))
	}

	body := c.payload[start:]
	if first && c.cutAfter < len(body) {
		body = body[:c.cutAfter]
	}

	if second && c.shortResume > 0 && c.shortResume < len(body) {
		body = body[:c.shortResume]
	}

	written, _ := writer.Write(body)

	c.mu.Lock()
	c.served += written
	c.mu.Unlock()

	if first && c.cutAfter < len(c.payload) {
		controller := http.NewResponseController(writer)
		_ = controller.Flush()

		conn, _, err := controller.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
}

func (c *cutting) snapshot() (ranges, ifRanges []string, served int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]string(nil), c.ranges...), append([]string(nil), c.ifRanges...), c.served
}

func (c *cutting) requests() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.ranges)
}

func serve(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return server
}

func randomPayload(t *testing.T, size int) []byte {
	t.Helper()

	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	return payload
}

// readAll drains the stream and closes it.
func readAll(t *testing.T, stream io.ReadCloser) ([]byte, error) {
	t.Helper()

	got, err := io.ReadAll(stream)

	if closeErr := stream.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	return got, err
}

// assertResumed holds the server's record to one cut and one resume from the
// byte reached: the second request asks for bytes=<cut>- under the first
// response's ETag, and the two bodies together are the payload, no byte twice.
func assertResumed(t *testing.T, handler *cutting) {
	t.Helper()

	ranges, ifRanges, served := handler.snapshot()
	if len(ranges) != 2 {
		t.Fatalf("%d requests, want 2: one cut, one resumed", len(ranges))
	}

	if want := "bytes=" + strconv.Itoa(handler.cutAfter) + "-"; ranges[1] != want {
		t.Fatalf("resumed request asked for %q, want %q", ranges[1], want)
	}

	if ifRanges[1] != etag {
		t.Fatalf("resumed request carried If-Range %q, want %s", ifRanges[1], etag)
	}

	if served != len(handler.payload) {
		t.Fatalf(
			"server sent %d bytes for a %d byte payload: resume did not start where the cut was",
			served,
			len(handler.payload),
		)
	}
}

func TestOpenResumesAfterCut(t *testing.T) {
	t.Parallel()

	payload := randomPayload(t, 3<<20)
	handler := &cutting{payload: payload, cutAfter: 1 << 20}
	server := serve(t, handler)

	var (
		mu      sync.Mutex
		reports []int64
	)

	stream, err := download.Open(t.Context(), server.URL, func(done, total int64) {
		mu.Lock()
		defer mu.Unlock()

		reports = append(reports, done)

		if total != int64(len(payload)) {
			t.Errorf("total %d, want %d", total, len(payload))
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := readAll(t, stream)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("content differs: %d bytes, want %d", len(got), len(payload))
	}

	assertResumed(t, handler)

	mu.Lock()
	defer mu.Unlock()

	if len(reports) < 2 || reports[0] != 0 || reports[len(reports)-1] != int64(len(payload)) {
		t.Fatalf("progress %v, want a first report at 0 and a last at %d", reports, len(payload))
	}
}

func TestOpenResumesABodyWithoutASize(t *testing.T) {
	t.Parallel()

	payload := randomPayload(t, 100_000)
	handler := &cutting{payload: payload, cutAfter: 40_000, sizeless: true}
	server := serve(t, handler)

	stream, err := download.Open(t.Context(), server.URL, func(_, total int64) {
		if total != -1 && total != int64(len(payload)) {
			t.Errorf("total %d, want -1 before the range answer and %d after", total, len(payload))
		}
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := readAll(t, stream)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("a cut body with no Content-Length was taken for complete: %d bytes, want %d", len(got), len(payload))
	}

	assertResumed(t, handler)
}

func TestOpenEndsASizelessBodyAtItsCleanEnd(t *testing.T) {
	t.Parallel()

	payload := randomPayload(t, 50_000)
	handler := &cutting{payload: payload, cutAfter: len(payload), sizeless: true}
	server := serve(t, handler)

	stream, err := download.Open(t.Context(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := readAll(t, stream)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("content differs: %d bytes, want %d", len(got), len(payload))
	}

	if ranges, _, _ := handler.snapshot(); len(ranges) != 1 {
		t.Fatalf("%d requests, want 1: a clean EOF is the end", len(ranges))
	}
}

func TestOpenDoesNotTakeAShortCleanEndForTheEnd(t *testing.T) {
	t.Parallel()

	for name, sizeless := range map[string]bool{
		"size from the first response": false,
		// The only size there is comes from the resumed 206's Content-Range,
		// and it must bound the rest: a short clean end here would hand the
		// store a truncated image to commit under its digest.
		"size from the range answer": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			payload := randomPayload(t, 100_000)
			handler := &cutting{payload: payload, cutAfter: 40_000, shortResume: 30_000, sizeless: sizeless}
			server := serve(t, handler)

			stream, err := download.Open(t.Context(), server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}

			got, err := readAll(t, stream)
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, payload) {
				t.Fatalf(
					"a clean EOF below the announced size was taken for the end: %d bytes, want %d",
					len(got),
					len(payload),
				)
			}

			ranges, _, served := handler.snapshot()
			if len(ranges) != 3 || ranges[2] != "bytes=70000-" {
				t.Fatalf("requests %v, want three with the last asking for bytes=70000-", ranges)
			}

			if served != len(payload) {
				t.Fatalf("server sent %d bytes, want %d", served, len(payload))
			}
		})
	}
}

func TestOpenRefusesARangeItDidNotAskFor(t *testing.T) {
	t.Parallel()

	for name, handler := range map[string]*cutting{
		"starting at the wrong byte": {misranged: true},
		"of another size":            {missized: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			payload := randomPayload(t, 100_000)
			handler.payload = payload
			handler.cutAfter = 40_000
			server := serve(t, handler)

			stream, err := download.Open(t.Context(), server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}

			got, err := readAll(t, stream)
			if !errors.Is(err, download.ErrDownload) || errors.Is(err, download.ErrChanged) {
				t.Fatalf("got %v, want ErrDownload for a 206 %s", err, name)
			}

			if !bytes.Equal(got, payload[:40_000]) {
				t.Fatalf("%d bytes delivered, want the 40000 before the cut and nothing from the wrong range", len(got))
			}

			if handler.requests() != 2 {
				t.Fatalf("%d requests, want 2: a wrong range is final", handler.requests())
			}
		})
	}
}

func TestOpenSkipsAWeakETagForIfRange(t *testing.T) {
	t.Parallel()

	payload := randomPayload(t, 100_000)
	handler := &cutting{payload: payload, cutAfter: 40_000, weak: true}
	server := serve(t, handler)

	stream, err := download.Open(t.Context(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := readAll(t, stream)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("content differs: %d bytes, want %d", len(got), len(payload))
	}

	if _, ifRanges, _ := handler.snapshot(); len(ifRanges) != 2 || ifRanges[1] != lastModified {
		t.Fatalf("If-Range %v, want Last-Modified %q on the resume, never the weak ETag", ifRanges, lastModified)
	}
}

func TestOpenRefusesAFileThatChanged(t *testing.T) {
	t.Parallel()

	payload := randomPayload(t, 1<<20)
	server := serve(t, &cutting{payload: payload, cutAfter: 4096, changed: true})

	stream, err := download.Open(t.Context(), server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := readAll(t, stream)
	if !errors.Is(err, download.ErrChanged) {
		t.Fatalf("got %v, want ErrChanged", err)
	}

	if !bytes.Equal(got, payload[:4096]) {
		t.Fatalf(
			"%d bytes delivered before the refusal, want the 4096 from the first response, nothing spliced",
			len(got),
		)
	}
}

func TestOpenFailsOnStatus(t *testing.T) {
	t.Parallel()

	server := serve(t, http.NotFoundHandler())

	_, err := download.Open(t.Context(), server.URL, nil)
	if !errors.Is(err, download.ErrDownload) {
		t.Fatalf("got %v, want ErrDownload", err)
	}
}

func TestOpenStopsWhenCancelledMidBody(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})

	server := serve(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "100000")
		_, _ = writer.Write(make([]byte, 4096))
		_ = http.NewResponseController(writer).Flush()

		// Hold the body open until the client has given up.
		select {
		case <-release:
		case <-request.Context().Done():
		}
	}))

	t.Cleanup(func() { close(release) })

	stream, err := download.Open(ctx, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}

	time.AfterFunc(100*time.Millisecond, cancel)

	got, err := readAll(t, stream)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	if len(got) != 4096 {
		t.Fatalf("%d bytes before the cancel, want the 4096 that were sent", len(got))
	}
}
