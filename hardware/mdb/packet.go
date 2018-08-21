package mdb

import (
	"bytes"
	"encoding/hex"
	"errors"
	"log"
	"strings"
)

const (
	PacketMaxLength = 40
)

var (
	ErrPacketOverflow = errors.New("mdb: operation larger than max packet size")
	ErrPacketReadonly = errors.New("mdb: packet is readonly")

	PacketEmpty = &Packet{readonly: true}
	PacketNul1  = &Packet{readonly: true, l: 1}
)

type Packet struct {
	b [PacketMaxLength]byte
	l int

	readonly bool
}

func PacketFromBytes(b []byte) *Packet {
	p := &Packet{}
	_, err := p.Write(b)
	if err != nil {
		return nil
	}
	return p
}

func PacketFromString(s string) *Packet { return PacketFromBytes([]byte(s)) }

func PacketFromHex(s string) *Packet {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return PacketFromBytes(b)
}

func (self *Packet) Bytes() []byte {
	return self.b[:self.l]
}

func (self *Packet) Equal(p2 *Packet) bool {
	return self.l == p2.l && bytes.Equal(self.Bytes(), p2.Bytes())
}

func (self *Packet) ReadFromPacket(src *Packet) error {
	_, err := self.Write(src.b[:src.l])
	return err
}

func (self *Packet) write(p []byte) {
	self.l = copy(self.b[:], p)
}

func (self *Packet) Write(p []byte) (n int, err error) {
	if self.readonly {
		return 0, ErrPacketReadonly
	}
	pl := len(p)
	switch {
	case pl == 0:
		return 0, nil
	case pl > PacketMaxLength:
		return 0, ErrPacketOverflow
	}
	self.write(p)
	return self.l, nil
}

func (self *Packet) Len() int { return self.l }

func (self *Packet) Logf(format string) {
	log.Printf(format, self.Format())
}

func (self *Packet) Format() string {
	b := self.Bytes()
	h := hex.EncodeToString(b)
	hlen := len(h)
	ss := make([]string, (hlen/8)+1)
	for i := range ss {
		hi := (i + 1) * 8
		if hi > hlen {
			hi = hlen
		}
		ss[i] = h[i*8 : hi]
	}
	line := strings.Join(ss, " ")
	return line
}

func (self *Packet) Wire(ffDance bool) []byte {
	chk := byte(0)
	for _, b := range self.b[:self.l] {
		chk += b
	}
	l := self.l + 1
	if ffDance {
		l += 2
	}
	w := make([]byte, l)
	copy(w, self.b[:self.l])
	if ffDance {
		w[l-3] = 0xff
		w[l-2] = 0x00
	}
	w[l-1] = chk
	return w
}