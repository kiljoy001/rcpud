# drawsrv Implementation Plan (Corrected)

**Goal:** Forward the Linux desktop (existing X11/Wayland session) to a 9front machine connected via rcpu -G.

**Architecture:**

The draw protocol (wsysmsg from drawfcall.h) is a raw binary message protocol over a Unix socket, NOT 9P files. plan9port's `devdraw -s srvname` already creates a socket and serves this protocol, but it opens its own small X11 window — it doesn't capture the existing desktop.

drawsrv is a replacement for plan9port's devdraw that:
1. Captures the existing Linux desktop pixels (PipeWire for Wayland, MIT-SHM for X11)
2. Serves a wsysmsg socket forwarding framebuffer updates to drawterm
3. Receives mouse/keyboard events from drawterm and injects them into Linux

rcpud proxies the drawsrv socket into the 9P namespace so drawterm on 9front can connect to it.

**Protocol (wsysmsg, defined in drawfcall.h):**

Binary messages over Unix socket:
```
uint32 length  (4 bytes big-endian)
uint8  type    (Tinit=14, Trdmouse=2, Trdkbd=10, Trddraw=20, Twrdraw=22, ...)
uint8  tag     (request tag)
[payload per type]
```

Key message types for desktop forwarding:
- Tinit → Rinit: Client connects, declares window size
- Trdmouse → Rrdmouse: Client requests mouse event (blocks until available)
- Trdkbd → Rrdkbd: Client requests keyboard event (blocks until available)
- Twrdraw: Server sends pixel data to client (framebuffer update)
- Trddraw → Rrddraw: Client requests pixel data (if pulling instead of pushing)

**Implementation tasks:**

### Task 1: wsysmsg protocol encoder/decoder in Go

Create the binary message serialization for Tinit, Rinit, Trdmouse, Rrdmouse, Trdkbd, Rrdkbd, Twrdraw, Trddraw, Rrddraw, Tlabel, Tresize, Ttop, Rtop.

Functions:
```
convM2W(data []byte, m *Wsysmsg) int
convW2M(m *Wsysmsg, buf []byte) int
sizeW2M(m *Wsysmsg) int
readwsysmsg(r io.Reader, buf []byte) int
```

### Task 2: Screen capture

Capture the Linux desktop as RGBA pixels:

- X11: Use MIT-SHM (`XShmGetImage`) for fast screen capture
- Wayland: Use PipeWire screencopy portal (`xdg-desktop-portal`) or `wlr-screencopy`
- Fallback: GDK/SDL pixel access

Send captured pixels as Twrdraw messages to the client.

### Task 3: Input injection

Mouse events arrive as Tmoveto (absolute position) and Rrdmouse (button state). Keyboard events arrive as Rrdkbd (rune).

Forward these to Linux:
- Mouse: `XWarpPointer` + `XTest` fake input for X11
- Mouse Wayland: `libei` or `uinput` virtual device
- Keyboard: `XTest` for X11, `uinput` for Wayland

### Task 4: wire into rcpud

Modify rcpud to forward the drawsrv socket into the per-session namespace. The drawterm client connects to the socket to speak wsysmsg directly.

### Alternative: Use plan9port devdraw directly

plan9port's devdraw -s drawsrv opens an X11 window and serves wsysmsg. It already handles mouse/kbd forwarding. If we just need to forward *any* X11 window (not the full desktop), this is zero code.

For full desktop capture, we'd use a separate screen capture tool (x11vnc with a custom pipe, or PipeWire → pixel pusher).
