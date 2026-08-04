// Package canbus is a tiny, dependency-free SocketCAN (AF_CAN) client.
//
// It uses only the Go standard library (syscall + unsafe for the bind sockaddr),
// so the whole dashboard builds as a fully static binary with no vendored modules
// -- which keeps the Yocto recipe trivial and reproducible.
//
// The raw socket is opened with CAN_RAW_FD_FRAMES enabled, so a single reader
// handles both classic CAN 2.0 frames (candrive) and CAN FD frames (candiag). read(2)
// returns 16 bytes for a classic frame and 72 for an FD frame; we switch on that.
package canbus

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	afCAN          = 29  // AF_CAN / PF_CAN
	protoCANRAW    = 1   // CAN_RAW
	solCANRAW      = 101 // SOL_CAN_RAW
	canRawFDFrames = 5   // CAN_RAW_FD_FRAMES sockopt

	canMTU   = 16 // sizeof(struct can_frame)
	canfdMTU = 72 // sizeof(struct canfd_frame)

	// can_id flag bits (top 3 bits of the 32-bit id field)
	flagEFF = 0x80000000 // extended (29-bit) frame
	flagRTR = 0x40000000 // remote transmission request
	flagERR = 0x20000000 // error frame
	maskSFF = 0x000007FF // standard 11-bit id
	maskEFF = 0x1FFFFFFF // extended 29-bit id

	// canfd_frame.flags
	fdBRS = 0x01 // bit-rate switch
	fdESI = 0x02 // error state indicator
)

// Frame is a decoded CAN frame.
type Frame struct {
	Time  time.Time
	Iface string
	ID    uint32
	Ext   bool // extended (29-bit) id
	RTR   bool
	Err   bool // error frame
	FD    bool // CAN FD frame
	BRS   bool // bit-rate switch (FD only)
	Data  []byte
}

// MarshalJSON emits the compact object the dashboard's SSE stream sends.
func (f Frame) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		T    float64 `json:"t"`
		If   string  `json:"if"`
		ID   string  `json:"id"`
		Ext  bool    `json:"ext"`
		RTR  bool    `json:"rtr"`
		Err  bool    `json:"err"`
		FD   bool    `json:"fd"`
		BRS  bool    `json:"brs"`
		Len  int     `json:"len"`
		Data string  `json:"data"`
	}{
		T:    float64(f.Time.UnixNano()) / 1e9,
		If:   f.Iface,
		ID:   fmt.Sprintf("%X", f.ID),
		Ext:  f.Ext,
		RTR:  f.RTR,
		Err:  f.Err,
		FD:   f.FD,
		BRS:  f.BRS,
		Len:  len(f.Data),
		Data: hex.EncodeToString(f.Data),
	})
}

// Conn is a bound raw CAN socket.
type Conn struct {
	fd    int
	iface string
}

func ifaceIndex(iface string) (int, error) {
	b, err := os.ReadFile("/sys/class/net/" + iface + "/ifindex")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(b)))
}

// Open binds a raw CAN socket to iface with FD frames enabled.
func Open(iface string) (*Conn, error) {
	idx, err := ifaceIndex(iface)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", iface, err)
	}
	fd, err := syscall.Socket(afCAN, syscall.SOCK_RAW, protoCANRAW)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}
	// Best-effort: enable FD frames. Harmless on classic-only interfaces.
	_ = syscall.SetsockoptInt(fd, solCANRAW, canRawFDFrames, 1)

	// struct sockaddr_can { u16 family; i32 ifindex; union{...} } == 16 bytes.
	var sa [16]byte
	binary.LittleEndian.PutUint16(sa[0:2], afCAN)
	binary.LittleEndian.PutUint32(sa[4:8], uint32(idx))
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&sa[0])), uintptr(len(sa))); errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("bind %s: %w", iface, errno)
	}
	return &Conn{fd: fd, iface: iface}, nil
}

// Close closes the socket (unblocks a pending ReadFrame).
func (c *Conn) Close() error { return syscall.Close(c.fd) }

// ReadFrame blocks until a frame arrives.
func (c *Conn) ReadFrame() (Frame, error) {
	buf := make([]byte, canfdMTU)
	n, err := syscall.Read(c.fd, buf)
	if err != nil {
		return Frame{}, err
	}
	raw := binary.LittleEndian.Uint32(buf[0:4])
	f := Frame{
		Time:  time.Now(),
		Iface: c.iface,
		Ext:   raw&flagEFF != 0,
		RTR:   raw&flagRTR != 0,
		Err:   raw&flagERR != 0,
	}
	if f.Ext {
		f.ID = raw & maskEFF
	} else {
		f.ID = raw & maskSFF
	}
	if n == canfdMTU {
		f.FD = true
		f.BRS = buf[5]&fdBRS != 0
		f.Data = clone(buf[8 : 8+clamp(int(buf[4]), 64)])
	} else { // classic frame (canMTU) or short read
		f.Data = clone(buf[8 : 8+clamp(int(buf[4]&0x0F), 8)])
	}
	return f, nil
}

// Send writes a frame (used by the canfeed generator). Writes an FD frame when
// f.FD is set, otherwise a classic frame.
func (c *Conn) Send(f Frame) error {
	raw := f.ID & maskSFF
	if f.Ext {
		raw = (f.ID & maskEFF) | flagEFF
	}
	if f.RTR {
		raw |= flagRTR
	}
	if f.FD {
		var out [canfdMTU]byte
		binary.LittleEndian.PutUint32(out[0:4], raw)
		out[4] = byte(clamp(len(f.Data), 64))
		if f.BRS {
			out[5] = fdBRS
		}
		copy(out[8:], f.Data)
		_, err := syscall.Write(c.fd, out[:])
		return err
	}
	var out [canMTU]byte
	binary.LittleEndian.PutUint32(out[0:4], raw)
	out[4] = byte(clamp(len(f.Data), 8))
	copy(out[8:], f.Data)
	_, err := syscall.Write(c.fd, out[:])
	return err
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

func clamp(n, hi int) int {
	if n < 0 {
		return 0
	}
	if n > hi {
		return hi
	}
	return n
}
