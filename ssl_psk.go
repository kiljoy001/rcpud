//go:build rcpud

package main

/*
#cgo LDFLAGS: -lssl -lcrypto

#include <stdlib.h>
#include <string.h>
#include <openssl/ssl.h>
#include <openssl/err.h>

static unsigned int psk_cb(SSL *ssl, const char *identity,
                           unsigned char *psk, unsigned int max_psk_len)
{
	unsigned char *secret = (unsigned char *)SSL_get_ex_data(ssl, 0);
	int secret_len = (int)(intptr_t)SSL_get_ex_data(ssl, 1);
	if (secret == NULL || secret_len <= 0 || (unsigned int)secret_len > max_psk_len)
		return 0;
	memcpy(psk, secret, secret_len);
	return (unsigned int)secret_len;
}

SSL_CTX *make_psk_ctx(void) {
	SSL_CTX *ctx = SSL_CTX_new(TLS_server_method());
	if (!ctx) return NULL;
	SSL_CTX_set_psk_server_callback(ctx, psk_cb);
	SSL_CTX_set_cipher_list(ctx, "PSK-AES128-CBC-SHA256:PSK-AES128-CBC-SHA");
	return ctx;
}

SSL *do_accept(int fd, unsigned char *secret, int secret_len) {
	SSL_CTX *ctx = make_psk_ctx();
	if (!ctx) return NULL;
	SSL *ssl = SSL_new(ctx);
	if (!ssl) { SSL_CTX_free(ctx); return NULL; }
	SSL_set_ex_data(ssl, 0, secret);
	SSL_set_ex_data(ssl, 1, (void *)(intptr_t)secret_len);
	SSL_set_fd(ssl, fd);
	int ret = SSL_accept(ssl);
	if (ret != 1) {
		unsigned long e = ERR_get_error();
		char buf[256];
		ERR_error_string_n(e, buf, sizeof(buf));
		fprintf(stderr, "ssl_accept: %s\n", buf);
		SSL_free(ssl); SSL_CTX_free(ctx); return NULL;
	}
	SSL_CTX_free(ctx);
	return ssl;
}

int do_read(SSL *ssl, void *buf, int n) {
	int r = SSL_read(ssl, buf, n);
	if (r <= 0) {
		int e = SSL_get_error(ssl, r);
		if (e == SSL_ERROR_ZERO_RETURN || e == SSL_ERROR_SYSCALL || e == SSL_ERROR_SSL)
			return -1;
	}
	return r;
}

int do_write(SSL *ssl, const void *buf, int n) {
	return SSL_write(ssl, buf, n);
}

void do_close(SSL *ssl) {
	if (ssl) { SSL_shutdown(ssl); SSL_free(ssl); }
}

void init_ossl(void) {
	SSL_library_init();
	SSL_load_error_strings();
	OpenSSL_add_all_algorithms();
}
*/
import "C"
import (
	"io"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"
)

func init() { C.init_ossl() }

type tlsConn struct {
	ssl    *C.SSL
	f      *os.File
	closed bool
	rmu    sync.Mutex
	wmu    sync.Mutex
	raddr  net.Addr
	laddr  net.Addr
}

func wrapTlsPsk(raw net.Conn, secret []byte) (*tlsConn, error) {
	tcp, ok := raw.(*net.TCPConn)
	if !ok {
		return nil, os.ErrInvalid
	}

	// tcp.File() returns a dup'd fd. We keep f alive so the fd stays valid
	// for OpenSSL. Don't close raw — shutdown() would kill our dup'd fd.
	f, err := tcp.File()
	if err != nil {
		return nil, err
	}

	laddr, raddr := raw.LocalAddr(), raw.RemoteAddr()
	fd := int(f.Fd())

	// Make C copy of secret for the handshake
	csecret := C.malloc(C.size_t(len(secret)))
	if csecret == nil {
		f.Close()
		return nil, os.ErrInvalid
	}
	C.memcpy(csecret, unsafe.Pointer(&secret[0]), C.size_t(len(secret)))

	ssl := C.do_accept(C.int(fd), (*C.uchar)(csecret), C.int(len(secret)))
	C.free(csecret)
	if ssl == nil {
		f.Close()
		return nil, os.ErrInvalid
	}

	return &tlsConn{
		ssl:   ssl,
		f:     f,
		raddr: raddr,
		laddr: laddr,
	}, nil
}

func (c *tlsConn) Read(b []byte) (int, error) {
	if c.closed || c.ssl == nil {
		return 0, io.EOF
	}
	c.rmu.Lock()
	defer c.rmu.Unlock()
	n := C.do_read(c.ssl, unsafe.Pointer(&b[0]), C.int(len(b)))
	if n < 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

func (c *tlsConn) Write(b []byte) (int, error) {
	if c.closed || c.ssl == nil {
		return 0, io.EOF
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	n := C.do_write(c.ssl, unsafe.Pointer(&b[0]), C.int(len(b)))
	if n <= 0 {
		return 0, io.EOF
	}
	return int(n), nil
}

func (c *tlsConn) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	if c.ssl != nil {
		C.do_close(c.ssl)
		c.ssl = nil
	}
	return c.f.Close()
}

func (c *tlsConn) LocalAddr() net.Addr               { return c.laddr }
func (c *tlsConn) RemoteAddr() net.Addr              { return c.raddr }
func (c *tlsConn) SetDeadline(t time.Time) error     { return nil }
func (c *tlsConn) SetReadDeadline(t time.Time) error { return nil }
func (c *tlsConn) SetWriteDeadline(t time.Time) error{ return nil }
