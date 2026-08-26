package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// peekLimit caps how much of an error body a heal inspects. Error payloads are
// always tiny; this is generous.
const healPeekLimit = 8 << 10

// closedReader preserves the original body's Close while exposing re-readable
// content. Heal paths replace resp.Body with a MultiReader (peeked prefix +
// remainder); wrapping that in io.NopCloser would permanently sever Close from
// the transport body — downstream ladder branches then "close" a no-op wrapper
// and the socket is never released for reuse. Delegating Close keeps the
// reattach invisible to every reader and closer in the pipeline.
type closedReader struct {
	io.Reader
	orig io.ReadCloser
}

func (c *closedReader) Close() error { return c.orig.Close() }

// healPeek reads up to healPeekLimit bytes of resp.Body and reattaches a
// re-readable, properly-closeable body (peeked bytes + unconsumed remainder).
// Every heal MUST reattach this way instead of io.NopCloser.
func healPeek(resp *http.Response) []byte {
	peeked, _ := io.ReadAll(io.LimitReader(resp.Body, healPeekLimit))
	resp.Body = &closedReader{
		Reader: io.MultiReader(bytes.NewReader(peeked), resp.Body),
		orig:   resp.Body,
	}
	return peeked
}

// healRedispatch builds and sends the healed request to the same upstream with
// the same credentials. Shared by every try*Heal so the request plumbing can't
// drift between them.
func (p *Proxy) healRedispatch(ctx context.Context, url, authKey string, body []byte) (*http.Response, error) {
	upReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Authorization", "Bearer "+authKey)
	upReq.Header.Set("Accept", "text/event-stream, application/json")
	upReq.Header.Set("X-Accel-Buffering", "no")
	return p.client.Do(upReq)
}

// healSucceeded reports whether a healed redispatch produced a 2xx. Learning is
// gated on this: a heal whose retry itself failed (500, cooldown-worthy 429…)
// proved nothing about the fix, so pinning the "answer" would be guesswork.
func healSucceeded(resp *http.Response) bool {
	return resp != nil && resp.StatusCode >= 200 && resp.StatusCode < 300
}
