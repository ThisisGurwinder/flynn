package proxy

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/flynn/flynn/pkg/random"
	router "github.com/flynn/flynn/router/types"
	"github.com/inconshreveable/log15"
	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/net/context"
)

type backendDialer interface {
	Dial(network, addr string) (c net.Conn, err error)
}

var (
	errNoBackends = errors.New("router: no backends available")
	errCanceled   = errors.New("router: backend connection canceled")

	httpTransport = &http.Transport{
		Dial: customDial,
		// The response header timeout is currently set pretty high because
		// gitreceive doesn't send headers until it is done unpacking the repo,
		// it should be lowered after this is fixed.
		ResponseHeaderTimeout: 10 * time.Minute,
		TLSHandshakeTimeout:   10 * time.Second, // unused, but safer to leave default in place
	}

	dialer backendDialer = &net.Dialer{
		Timeout:   1 * time.Second,
		KeepAlive: 30 * time.Second,
	}
)

// BackendListFunc returns a slice of backends
type BackendListFunc func() []*router.Backend

type transport struct {
	getBackends BackendListFunc

	stickyCookieKey   *[32]byte
	useStickySessions bool
}

func (t *transport) getOrderedBackends(stickyBackend string) []*router.Backend {
	backends := t.getBackends()
	shuffleBackends(backends)

	if stickyBackend != "" {
		swapToFront(backends, stickyBackend)
	}
	return backends
}

func (t *transport) getStickyBackend(req *http.Request) string {
	if t.useStickySessions {
		return getStickyCookieBackend(req, *t.stickyCookieKey)
	}
	return ""
}

func (t *transport) setStickyBackend(res *http.Response, originalStickyBackend string) {
	if !t.useStickySessions {
		return
	}
	if backend := res.Request.URL.Host; backend != originalStickyBackend {
		setStickyCookieBackend(res, backend, *t.stickyCookieKey)
	}
}

func (t *transport) RoundTrip(ctx context.Context, req *http.Request, l log15.Logger) (*http.Response, string, error) {
	// http.Transport closes the request body on a failed dial, issue #875
	req.Body = &fakeCloseReadCloser{req.Body}
	defer req.Body.(*fakeCloseReadCloser).RealClose()

	// hook up CloseNotify to cancel the request
	req.Cancel = ctx.Done()

	rt := ctx.Value(ctxKeyRequestTracker).(RequestTracker)
	stickyBackend := t.getStickyBackend(req)
	backends := t.getOrderedBackends(stickyBackend)
	for i, backend := range backends {
		req.URL.Host = backend.Addr
		rt.TrackRequestStart(backend.Addr)
		res, err := httpTransport.RoundTrip(req)
		if err == nil {
			t.setStickyBackend(res, stickyBackend)
			return res, backend.Addr, nil
		}
		rt.TrackRequestDone(backend.Addr)
		if _, ok := err.(dialErr); !ok {
			l.Error("unretriable request error", "service", backend.Service, "job.id", backend.JobID, "addr", backend.Addr, "err", err, "attempt", i)
			return nil, "", err
		}
		l.Error("retriable dial error", "service", backend.Service, "job.id", backend.JobID, "addr", backend.Addr, "err", err, "attempt", i)
	}
	l.Error("request failed", "status", "503", "num_backends", len(backends))
	return nil, "", errNoBackends
}

func (t *transport) Connect(ctx context.Context, l log15.Logger) (net.Conn, error) {
	backends := t.getOrderedBackends("")
	conn, _, err := dialTCP(ctx, l, backends)
	if err != nil {
		l.Error("connection failed", "num_backends", len(backends))
	}
	return conn, err
}

func (t *transport) UpgradeHTTP(req *http.Request, l log15.Logger) (*http.Response, net.Conn, error) {
	stickyBackend := t.getStickyBackend(req)
	backends := t.getOrderedBackends(stickyBackend)
	upconn, addr, err := dialTCP(context.Background(), l, backends)
	if err != nil {
		l.Error("dial failed", "status", "503", "num_backends", len(backends))
		return nil, nil, err
	}
	conn := &streamConn{bufio.NewReader(upconn), upconn}
	req.URL.Host = addr

	if err := req.Write(conn); err != nil {
		conn.Close()
		l.Error("error writing request", "err", err, "backend", addr)
		return nil, nil, err
	}
	res, err := http.ReadResponse(conn.Reader, req)
	if err != nil {
		conn.Close()
		l.Error("error reading response", "err", err, "backend", addr)
		return nil, nil, err
	}
	t.setStickyBackend(res, stickyBackend)
	return res, conn, nil
}

func dialTCP(ctx context.Context, l log15.Logger, backends []*router.Backend) (net.Conn, string, error) {
	donec := ctx.Done()
	for i, backend := range backends {
		select {
		case <-donec:
			return nil, "", errCanceled
		default:
		}
		conn, err := dialer.Dial("tcp", backend.Addr)
		if err == nil {
			return conn, backend.Addr, nil
		}
		l.Error("retriable dial error", "service", backend.Service, "job.id", backend.JobID, "addr", backend.Addr, "err", err, "attempt", i)
	}
	return nil, "", errNoBackends
}

func customDial(network, addr string) (net.Conn, error) {
	conn, err := dialer.Dial(network, addr)
	if err != nil {
		return nil, dialErr{err}
	}
	return conn, nil
}

type dialErr struct {
	error
}

type fakeCloseReadCloser struct {
	io.ReadCloser
}

func (w *fakeCloseReadCloser) Close() error {
	return nil
}

func (w *fakeCloseReadCloser) RealClose() error {
	if w.ReadCloser == nil {
		return nil
	}
	return w.ReadCloser.Close()
}

func shuffleBackends(backends []*router.Backend) {
	for i := len(backends) - 1; i > 0; i-- {
		j := random.Math.Intn(i + 1)
		backends[i], backends[j] = backends[j], backends[i]
	}
}

func swapToFront(backends []*router.Backend, addr string) {
	for i, backend := range backends {
		if backend.Addr == addr {
			backends[0], backends[i] = backends[i], backends[0]
			return
		}
	}
}

func getStickyCookieBackend(req *http.Request, cookieKey [32]byte) string {
	cookie, err := req.Cookie(stickyCookie)
	if err != nil {
		return ""
	}

	data, err := base64.StdEncoding.DecodeString(cookie.Value)
	if err != nil {
		return ""
	}
	return string(decrypt(data, cookieKey))
}

func setStickyCookieBackend(res *http.Response, backend string, cookieKey [32]byte) {
	cookie := http.Cookie{
		Name:  stickyCookie,
		Value: base64.StdEncoding.EncodeToString(encrypt([]byte(backend), cookieKey)),
		Path:  "/",
	}
	res.Header.Add("Set-Cookie", cookie.String())
}

func encrypt(data []byte, key [32]byte) []byte {
	var nonce [24]byte
	_, err := io.ReadFull(rand.Reader, nonce[:])
	if err != nil {
		panic(err)
	}

	out := make([]byte, len(nonce), len(nonce)+len(data)+secretbox.Overhead)
	copy(out, nonce[:])
	return secretbox.Seal(out, data, &nonce, &key)
}

func decrypt(data []byte, key [32]byte) []byte {
	var nonce [24]byte
	if len(data) < len(nonce) {
		return nil
	}
	copy(nonce[:], data)
	res, ok := secretbox.Open(nil, data[len(nonce):], &nonce, &key)
	if !ok {
		return nil
	}
	return res
}
