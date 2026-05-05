package auth

/*
#cgo CFLAGS: -I/home/scott/Repo/go-dp9ik/drawterm/include -I/home/scott/Repo/go-dp9ik/drawterm
#cgo LDFLAGS: -Wl,--start-group
#cgo LDFLAGS: /home/scott/Repo/go-dp9ik/drawterm/libauthsrv/libauthsrv.a
#cgo LDFLAGS: /home/scott/Repo/go-dp9ik/drawterm/libsec/libsec.a
#cgo LDFLAGS: /home/scott/Repo/go-dp9ik/drawterm/libmp/libmp.a
#cgo LDFLAGS: /home/scott/Repo/go-dp9ik/drawterm/libc/libc.a
#cgo LDFLAGS: /home/scott/Repo/go-dp9ik/drawterm/libmachdep.a
#cgo LDFLAGS: -Wl,--end-group
#cgo LDFLAGS: -lm -lpthread

#include "u.h"
#include "libc.h"
#include "authsrv.h"
*/
import "C"
import "unsafe"

const (
	ANAMELEN  = 28
	DOMLEN    = 48
	CHALLEN   = 8
	NONCELEN  = 32
	AuthPAK   = 19
	TICKREQLEN = 141
)

// Ticketreq matches the C struct Ticketreq
type Ticketreq struct {
	Type    byte
	Authid  [ANAMELEN]byte
	Authdom [DOMLEN]byte
	Chal    [CHALLEN]byte
	Hostid  [ANAMELEN]byte
	Uid     [ANAMELEN]byte
}

// Marshal converts Ticketreq to wire format using C implementation
func (tr *Ticketreq) Marshal() ([]byte, error) {
	var ctr C.Ticketreq
	
	// Set type field - access first byte of struct
	*(*C.char)(unsafe.Pointer(&ctr)) = C.char(tr.Type)
	
	// Copy array fields using unsafe pointer casts
	copy((*[ANAMELEN]byte)(unsafe.Pointer(&ctr.authid))[:], tr.Authid[:])
	copy((*[DOMLEN]byte)(unsafe.Pointer(&ctr.authdom))[:], tr.Authdom[:])
	copy((*[CHALLEN]byte)(unsafe.Pointer(&ctr.chal))[:], tr.Chal[:])
	copy((*[ANAMELEN]byte)(unsafe.Pointer(&ctr.hostid))[:], tr.Hostid[:])
	copy((*[ANAMELEN]byte)(unsafe.Pointer(&ctr.uid))[:], tr.Uid[:])
	
	buf := make([]byte, TICKREQLEN)
	n := C.convTR2M(&ctr, (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	
	if n <= 0 {
		return nil, &AuthError{Msg: "convTR2M failed"}
	}
	
	return buf[:n], nil
}

// AuthError represents an authentication error
type AuthError struct {
	Msg string
}

func (e *AuthError) Error() string {
	return e.Msg
}
