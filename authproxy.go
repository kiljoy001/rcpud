//go:build rcpud

package main

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Plan9-Archive/libauth"
)

// exactReader wraps an io.Reader so that Read(p) only returns
// exactly len(p) bytes (like io.ReadFull). This is needed because
// the factotum RPC protocol expects byte-exact reads — TCP can
// coalesce multiple messages into one segment.
type exactReader struct {
	r io.Reader
}

func (e *exactReader) Read(p []byte) (int, error) {
	return io.ReadFull(e.r, p)
}

// proxyAuth is a drop-in replacement for libauth.Proxy that fixes
// the phase-read bug: in ARphase, it reads EXACTLY the number of bytes
// factotum asks for, rather than reading all available TCP data.
func proxyAuth(rw io.ReadWriter, format string, a ...interface{}) (*AuthInfo, error) {
	f, err := libauth.OpenRPC()
	if err != nil {
		return nil, fmt.Errorf("openRPC: %w", err)
	}
	defer f.Close()

	keyspec := fmt.Sprintf(format, a...)
	return fauthProxy(rw, f, keyspec)
}

// AuthInfo mirrors libauth.AuthInfo for convenience
type AuthInfo struct {
	Cuid   string
	Suid   string
	Cap    string
	Secret []byte
}

type authRpc struct {
	f   io.ReadWriteCloser
	arg []byte
	ai  *AuthInfo
}

const authRpcMax = 4096

type authRet int

const (
	arOK       authRet = iota
	arDone
	arError
	arNeedkey
	arBadkey
	arTooSmall
	arRpcFailure
	arPhase
)

var arTab = map[string]authRet{
	"ok":       arOK,
	"done":     arDone,
	"error":    arError,
	"needkey":  arNeedkey,
	"badkey":   arBadkey,
	"phase":    arPhase,
	"toosmall": arTooSmall,
}

func (r *authRpc) rpc(verb string, arg string) (authRet, string) {
	if len(verb)+1+len(arg) > authRpcMax {
		return arRpcFailure, "rpc too big"
	}
	if _, err := r.f.Write([]byte(verb + " " + arg)); err != nil {
		return arRpcFailure, "write: " + err.Error()
	}
	ibuf := make([]byte, authRpcMax)
	n, err := r.f.Read(ibuf)
	if err != nil {
		return arRpcFailure, "read: " + err.Error()
	}
	ibuf = ibuf[:n]

	var iverb string
	if i := bytes.IndexByte(ibuf, ' '); i > 0 {
		iverb = string(ibuf[:i])
		r.arg = ibuf[i+1:]
	} else {
		iverb = string(ibuf)
		r.arg = nil
	}

	ar, ok := arTab[iverb]
	if !ok {
		return arRpcFailure, "malformed rpc response: " + string(ibuf)
	}
	switch ar {
	case arOK:
		return arOK, string(r.arg)
	case arDone:
		return arDone, string(r.arg)
	case arError:
		if len(r.arg) == 0 {
			return arError, "unspecified rpc error"
		}
		return arError, string(r.arg)
	case arPhase:
		return arPhase, "phase error " + string(r.arg)
	case arTooSmall:
		return arTooSmall, string(r.arg)
	default:
		return ar, fmt.Sprintf("unknown rpc type %d", ar)
	}
}

func (r *authRpc) getInfo() error {
	if ret, msg := r.rpc("authinfo", ""); ret != arOK {
		return fmt.Errorf("authinfo: %s", msg)
	}
	ai := convM2AI(r.arg)
	if ai == nil {
		return fmt.Errorf("bad auth info from factotum")
	}
	r.ai = ai
	return nil
}

func convM2AI(buf []byte) *AuthInfo {
	ai := new(AuthInfo)

	buf, ai.Cuid = gstring(buf)
	buf, ai.Suid = gstring(buf)
	buf, ai.Cap = gstring(buf)
	buf, ai.Secret = garray(buf)

	return ai
}

func gbit16(p []byte) uint16 {
	return uint16(p[0]) | uint16(p[1])<<8
}

func gstring(buf []byte) ([]byte, string) {
	if len(buf) < 2 {
		return buf, ""
	}
	n := int(gbit16(buf))
	buf = buf[2:]
	if len(buf) < n {
		return buf, ""
	}
	return buf[n:], string(buf[:n])
}

func garray(buf []byte) ([]byte, []byte) {
	if len(buf) < 2 {
		return buf, nil
	}
	n := int(gbit16(buf))
	buf = buf[2:]
	if len(buf) < n {
		return buf, nil
	}
	return buf[n:], buf[:n]
}

// fauthProxy - fixed version that reads exact byte counts in phase mode
func fauthProxy(rw io.ReadWriter, rpc io.ReadWriteCloser, params string) (*AuthInfo, error) {
	r := &authRpc{f: rpc}

	// start
	if ret, msg := r.rpc("start", params); ret != arOK {
		return nil, fmt.Errorf("fauth_proxy start: %s", msg)
	}

	buf := make([]byte, authRpcMax)
	for {
		ret, msg := r.rpc("read", "")
		switch ret {
		case arDone:
			if err := r.getInfo(); err != nil {
				return nil, err
			}
			return r.ai, nil

		case arOK:
			// factotum has data to send to wire
			if _, err := rw.Write(r.arg); err != nil {
				return nil, fmt.Errorf("write to wire: %w", err)
			}

		case arPhase:
			// factotum needs data from wire — read exactly what it asks for
			n := 0
			for {
				// Try to write what we've read so far
				ret2, msg2 := r.rpc("write", string(buf[:n]))
				if ret2 != arTooSmall {
					if ret2 != arOK {
						return nil, fmt.Errorf("phase write: %s", msg2)
					}
					break // got OK, factotum accepted the data
				}

				// factotum wants more bytes
				needStr := strings.TrimSpace(string(r.arg))
				need, err := strconv.Atoi(needStr)
				if err != nil {
					return nil, fmt.Errorf("phase atoi: %s: %s", err.Error(), needStr)
				}
				if need > authRpcMax {
					return nil, fmt.Errorf("factotum wants %d bytes, max is %d", need, authRpcMax)
				}

				// KEY FIX: Read EXACTLY (need - n) bytes, not whatever is available
				toRead := need - n
				if toRead <= 0 {
					return nil, fmt.Errorf("factotum wants %d but we already have %d", need, n)
				}
				_, err = io.ReadFull(rw, buf[n:need])
				if err != nil {
					return nil, fmt.Errorf("phase read from wire: %w", err)
				}
				n = need
			}

		default:
			return nil, fmt.Errorf("rpc: %s", msg)
		}
	}
}
