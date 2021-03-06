// Copyright (C) 2014 Space Monkey, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build cgo

package openssl

// #include <stdlib.h>
// #include <openssl/ssl.h>
// #include <openssl/conf.h>
// #include <openssl/err.h>
//
// int sk_X509_num_not_a_macro(STACK_OF(X509) *sk) { return sk_X509_num(sk); }
// X509 *sk_X509_value_not_a_macro(STACK_OF(X509)* sk, int i) {
//    return sk_X509_value(sk, i);
// }
// long SSL_set_tlsext_host_name_not_a_macro(SSL *ssl, const char *name) {
//    return SSL_set_tlsext_host_name(ssl, name);
// }
import "C"

import (
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/spacemonkeygo/openssl/utils"
)

var (
	zeroReturn = errors.New("zero return")
	wantRead   = errors.New("want read")
	wantWrite  = errors.New("want write")
	tryAgain   = errors.New("try again")
)

type Conn struct {
	conn             net.Conn
	ssl              *C.SSL
	ctx              *Ctx // for gc
	into_ssl         *readBio
	from_ssl         *writeBio
	is_shutdown      bool
	mtx              sync.Mutex
	want_read_future *utils.Future
}

func newSSL(ctx *C.SSL_CTX) (*C.SSL, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ssl := C.SSL_new(ctx)
	if ssl == nil {
		return nil, errorFromErrorQueue()
	}
	return ssl, nil
}

func newConn(conn net.Conn, ctx *Ctx) (*Conn, error) {
	ssl, err := newSSL(ctx.ctx)
	if err != nil {
		return nil, err
	}

	into_ssl := &readBio{}
	from_ssl := &writeBio{}

	if ctx.GetMode()&ReleaseBuffers > 0 {
		into_ssl.release_buffers = true
		from_ssl.release_buffers = true
	}

	into_ssl_cbio := into_ssl.MakeCBIO()
	from_ssl_cbio := from_ssl.MakeCBIO()
	if into_ssl_cbio == nil || from_ssl_cbio == nil {
		// these frees are null safe
		C.BIO_free(into_ssl_cbio)
		C.BIO_free(from_ssl_cbio)
		C.SSL_free(ssl)
		return nil, errors.New("failed to allocate memory BIO")
	}

	// the ssl object takes ownership of these objects now
	C.SSL_set_bio(ssl, into_ssl_cbio, from_ssl_cbio)

	c := &Conn{
		conn:     conn,
		ssl:      ssl,
		ctx:      ctx,
		into_ssl: into_ssl,
		from_ssl: from_ssl}
	runtime.SetFinalizer(c, func(c *Conn) {
		c.into_ssl.Disconnect(into_ssl_cbio)
		c.from_ssl.Disconnect(from_ssl_cbio)
		C.SSL_free(c.ssl)
	})
	return c, nil
}

// Client wraps an existing stream connection and puts it in the connect state
// for any subsequent handshakes.
//
// IMPORTANT NOTE: if you use this method instead of Dial to construct an SSL
// connection, you are responsible for verifying the peer's hostname.
// Otherwise, you are vulnerable to MITM attacks.
//
// Client also does not set up SNI for you like Dial does.
//
// Client connections probably won't work for you unless you set a verify
// location or add some certs to the certificate store of the client context
// you're using. This library is not nice enough to use the system certificate
// store by default for you yet.
func Client(conn net.Conn, ctx *Ctx) (*Conn, error) {
	c, err := newConn(conn, ctx)
	if err != nil {
		return nil, err
	}
	C.SSL_set_connect_state(c.ssl)
	return c, nil
}

// Server wraps an existing stream connection and puts it in the accept state
// for any subsequent handshakes.
func Server(conn net.Conn, ctx *Ctx) (*Conn, error) {
	c, err := newConn(conn, ctx)
	if err != nil {
		return nil, err
	}
	C.SSL_set_accept_state(c.ssl)
	return c, nil
}

func (c *Conn) fillInputBuffer() error {
	for {
		n, err := c.into_ssl.ReadFromOnce(c.conn)
		if n == 0 && err == nil {
			continue
		}
		if err == io.EOF {
			c.into_ssl.MarkEOF()
			return c.Close()
		}
		return err
	}
}

func (c *Conn) flushOutputBuffer() error {
	_, err := c.from_ssl.WriteTo(c.conn)
	return err
}

func (c *Conn) getErrorHandler(rv C.int, errno error) func() error {
	errcode := C.SSL_get_error(c.ssl, rv)
	switch errcode {
	case C.SSL_ERROR_ZERO_RETURN:
		return func() error {
			c.Close()
			return io.ErrUnexpectedEOF
		}
	case C.SSL_ERROR_WANT_READ:
		go c.flushOutputBuffer()
		if c.want_read_future != nil {
			want_read_future := c.want_read_future
			return func() error {
				_, err := want_read_future.Get()
				return err
			}
		}
		c.want_read_future = utils.NewFuture()
		want_read_future := c.want_read_future
		return func() (err error) {
			defer func() {
				c.mtx.Lock()
				c.want_read_future = nil
				c.mtx.Unlock()
				want_read_future.Set(nil, err)
			}()
			err = c.fillInputBuffer()
			if err != nil {
				return err
			}
			return tryAgain
		}
	case C.SSL_ERROR_WANT_WRITE:
		return func() error {
			err := c.flushOutputBuffer()
			if err != nil {
				return err
			}
			return tryAgain
		}
	case C.SSL_ERROR_SYSCALL:
		var err error
		if C.ERR_peek_error() == 0 {
			switch rv {
			case 0:
				err = errors.New("protocol-violating EOF")
			case -1:
				err = errno
			default:
				err = errorFromErrorQueue()
			}
		} else {
			err = errorFromErrorQueue()
		}
		return func() error { return err }
	default:
		err := errorFromErrorQueue()
		return func() error { return err }
	}
}

func (c *Conn) handleError(errcb func() error) error {
	if errcb != nil {
		return errcb()
	}
	return nil
}

func (c *Conn) handshake() func() error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.is_shutdown {
		return func() error { return io.ErrUnexpectedEOF }
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rv, errno := C.SSL_do_handshake(c.ssl)
	if rv > 0 {
		return nil
	}
	return c.getErrorHandler(rv, errno)
}

// Handshake performs an SSL handshake. If a handshake is not manually
// triggered, it will run before the first I/O on the encrypted stream.
func (c *Conn) Handshake() error {
	err := tryAgain
	for err == tryAgain {
		err = c.handleError(c.handshake())
	}
	go c.flushOutputBuffer()
	return err
}

// PeerCertificate returns the Certificate of the peer with which you're
// communicating. Only valid after a handshake.
func (c *Conn) PeerCertificate() (*Certificate, error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.is_shutdown {
		return nil, errors.New("connection closed")
	}
	x := C.SSL_get_peer_certificate(c.ssl)
	if x == nil {
		return nil, errors.New("no peer certificate found")
	}
	cert := &Certificate{x: x}
	runtime.SetFinalizer(cert, func(cert *Certificate) {
		C.X509_free(cert.x)
	})
	return cert, nil
}

// PeerCertificateChain returns the certificate chain of the peer. If called on
// the client side, the stack also contains the peer's certificate; if called
// on the server side, the peer's certificate must be obtained separately using
// PeerCertificate.
func (c *Conn) PeerCertificateChain() (rv []*Certificate, err error) {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.is_shutdown {
		return nil, errors.New("connection closed")
	}
	sk := C.SSL_get_peer_cert_chain(c.ssl)
	if sk == nil {
		return nil, errors.New("no peer certificates found")
	}
	sk_num := int(C.sk_X509_num_not_a_macro(sk))
	rv = make([]*Certificate, 0, sk_num)
	for i := 0; i < sk_num; i++ {
		x := C.sk_X509_value_not_a_macro(sk, C.int(i))
		// ref holds on to the underlying connection memory so we don't need to
		// worry about incrementing refcounts manually or freeing the X509
		rv = append(rv, &Certificate{x: x, ref: c})
	}
	return rv, nil
}

type ConnectionState struct {
	Certificate           *Certificate
	CertificateError      error
	CertificateChain      []*Certificate
	CertificateChainError error
}

func (c *Conn) ConnectionState() (rv ConnectionState) {
	rv.Certificate, rv.CertificateError = c.PeerCertificate()
	rv.CertificateChain, rv.CertificateChainError = c.PeerCertificateChain()
	return
}

func (c *Conn) shutdown() func() error {
	c.mtx.Lock()
	defer c.mtx.Unlock()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rv, errno := C.SSL_shutdown(c.ssl)
	if rv > 0 {
		return nil
	}
	if rv == 0 {
		// The OpenSSL docs say that in this case, the shutdown is not
		// finished, and we should call SSL_shutdown() a second time, if a
		// bidirectional shutdown is going to be performed. Further, the
		// output of SSL_get_error may be misleading, as an erroneous
		// SSL_ERROR_SYSCALL may be flagged even though no error occurred.
		// So, TODO: revisit bidrectional shutdown, possibly trying again.
		// Note: some broken clients won't engage in bidirectional shutdown
		// without tickling them to close by sending a TCP_FIN packet, or
		// shutting down the write-side of the connection.
		return nil
	} else {
		return c.getErrorHandler(rv, errno)
	}
}

func (c *Conn) shutdownLoop() error {
	err := tryAgain
	shutdown_tries := 0
	for err == tryAgain {
		shutdown_tries = shutdown_tries + 1
		err = c.handleError(c.shutdown())
		if err == nil {
			return c.flushOutputBuffer()
		}
		if err == tryAgain && shutdown_tries >= 2 {
			return errors.New("shutdown requested a third time?")
		}
	}
	if err == io.ErrUnexpectedEOF {
		err = nil
	}
	return err
}

// Close shuts down the SSL connection and closes the underlying wrapped
// connection.
func (c *Conn) Close() error {
	c.mtx.Lock()
	if c.is_shutdown {
		c.mtx.Unlock()
		return nil
	}
	c.is_shutdown = true
	c.mtx.Unlock()
	var errs utils.ErrorGroup
	errs.Add(c.shutdownLoop())
	errs.Add(c.conn.Close())
	return errs.Finalize()
}

func (c *Conn) read(b []byte) (int, func() error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.is_shutdown {
		return 0, func() error { return io.EOF }
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rv, errno := C.SSL_read(c.ssl, unsafe.Pointer(&b[0]), C.int(len(b)))
	if rv > 0 {
		return int(rv), nil
	}
	return 0, c.getErrorHandler(rv, errno)
}

// Read reads up to len(b) bytes into b. It returns the number of bytes read
// and an error if applicable. io.EOF is returned when the caller can expect
// to see no more data.
func (c *Conn) Read(b []byte) (n int, err error) {
	if len(b) == 0 {
		return 0, nil
	}
	err = tryAgain
	for err == tryAgain {
		n, errcb := c.read(b)
		err = c.handleError(errcb)
		if err == nil {
			go c.flushOutputBuffer()
			return n, nil
		}
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
	}
	return 0, err
}

func (c *Conn) write(b []byte) (int, func() error) {
	if len(b) == 0 {
		return 0, nil
	}
	c.mtx.Lock()
	defer c.mtx.Unlock()
	if c.is_shutdown {
		err := errors.New("connection closed")
		return 0, func() error { return err }
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	rv, errno := C.SSL_write(c.ssl, unsafe.Pointer(&b[0]), C.int(len(b)))
	if rv > 0 {
		return int(rv), nil
	}
	return 0, c.getErrorHandler(rv, errno)
}

// Write will encrypt the contents of b and write it to the underlying stream.
// Performance will be vastly improved if the size of b is a multiple of
// SSLRecordSize.
func (c *Conn) Write(b []byte) (written int, err error) {
	if len(b) == 0 {
		return 0, nil
	}
	err = tryAgain
	for err == tryAgain {
		n, errcb := c.write(b)
		err = c.handleError(errcb)
		if err == nil {
			return n, c.flushOutputBuffer()
		}
	}
	return 0, err
}

// VerifyHostname pulls the PeerCertificate and calls VerifyHostname on the
// certificate.
func (c *Conn) VerifyHostname(host string) error {
	cert, err := c.PeerCertificate()
	if err != nil {
		return err
	}
	return cert.VerifyHostname(host)
}

// LocalAddr returns the underlying connection's local address
func (c *Conn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr returns the underlying connection's remote address
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline calls SetDeadline on the underlying connection.
func (c *Conn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// SetReadDeadline calls SetReadDeadline on the underlying connection.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SetWriteDeadline calls SetWriteDeadline on the underlying connection.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *Conn) UnderlyingConn() net.Conn {
	return c.conn
}

func (c *Conn) SetTlsExtHostName(name string) error {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if C.SSL_set_tlsext_host_name_not_a_macro(c.ssl, cname) == 0 {
		return errorFromErrorQueue()
	}
	return nil
}
