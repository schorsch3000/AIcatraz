package egress

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
)

// DNS record and response codes this package cares about.
const (
	TypeA     = 1
	TypeCNAME = 5
	TypeAAAA  = 28

	rcodeOK       = 0
	rcodeNXDomain = 3
)

// Query is the first question of a DNS message.
type Query struct {
	ID   uint16
	Name string // lower case, no trailing dot
	Type uint16
}

// Record is an answer record, reduced to what the forwarder needs.
type Record struct {
	Name  string // owner name
	Type  uint16
	TTL   uint32
	IP    netip.Addr // for A records
	Alias string     // for CNAME records
}

var errShort = errors.New("dns: message too short")

// parseName reads a possibly compressed name at off and returns it with the
// offset of the next field.
func parseName(msg []byte, off int) (string, int, error) {
	var labels []string
	next := -1
	for hops := 0; hops < 32; hops++ {
		if off >= len(msg) {
			return "", 0, errShort
		}
		n := int(msg[off])
		switch {
		case n == 0:
			if next < 0 {
				next = off + 1
			}
			return strings.ToLower(strings.Join(labels, ".")), next, nil
		case n&0xc0 == 0xc0: // a pointer
			if off+1 >= len(msg) {
				return "", 0, errShort
			}
			if next < 0 {
				next = off + 2
			}
			off = (n&0x3f)<<8 | int(msg[off+1])
		case n&0xc0 != 0:
			return "", 0, errors.New("dns: bad label")
		default:
			if off+1+n > len(msg) {
				return "", 0, errShort
			}
			labels = append(labels, string(msg[off+1:off+1+n]))
			off += 1 + n
		}
	}
	return "", 0, errors.New("dns: name loops")
}

// ParseQuery reads the first question of a message.
func ParseQuery(msg []byte) (Query, error) {
	if len(msg) < 12 {
		return Query{}, errShort
	}
	if binary.BigEndian.Uint16(msg[4:6]) < 1 {
		return Query{}, errors.New("dns: no question")
	}
	name, off, err := parseName(msg, 12)
	if err != nil {
		return Query{}, err
	}
	if off+4 > len(msg) {
		return Query{}, errShort
	}
	return Query{ID: binary.BigEndian.Uint16(msg[0:2]), Name: name, Type: binary.BigEndian.Uint16(msg[off : off+2])}, nil
}

// ParseResponse reads the answer records of a response.
func ParseResponse(msg []byte) (q Query, rcode int, recs []Record, err error) {
	if q, err = ParseQuery(msg); err != nil {
		return
	}
	rcode = int(msg[3] & 0x0f)
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	_, off, err := parseName(msg, 12)
	if err != nil {
		return
	}
	off += 4
	for i := 0; i < an; i++ {
		var name string
		if name, off, err = parseName(msg, off); err != nil {
			return
		}
		if off+10 > len(msg) {
			err = errShort
			return
		}
		r := Record{Name: name, Type: binary.BigEndian.Uint16(msg[off : off+2]), TTL: binary.BigEndian.Uint32(msg[off+4 : off+8])}
		rdlen := int(binary.BigEndian.Uint16(msg[off+8 : off+10]))
		off += 10
		if off+rdlen > len(msg) {
			err = errShort
			return
		}
		switch r.Type {
		case TypeA:
			if rdlen == 4 {
				r.IP = netip.AddrFrom4([4]byte(msg[off : off+4]))
			}
		case TypeCNAME:
			if alias, _, perr := parseName(msg, off); perr == nil {
				r.Alias = alias
			}
		}
		recs = append(recs, r)
		off += rdlen
	}
	return
}

// BuildQuery makes a query message for one name, for tests and probes.
func BuildQuery(id uint16, name string, qtype uint16) []byte {
	b := make([]byte, 12, 32+len(name))
	binary.BigEndian.PutUint16(b[0:2], id)
	b[2] = 0x01 // recursion desired
	binary.BigEndian.PutUint16(b[4:6], 1)
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	b = append(b, 0, 0, 0, 0, 1) // root label, type placeholder, class IN
	binary.BigEndian.PutUint16(b[len(b)-4:len(b)-2], qtype)
	return b
}

// reply makes a response to query with the given response code and no answers.
func reply(query []byte, rcode int) []byte {
	if len(query) < 12 {
		return nil
	}
	_, off, err := parseName(query, 12)
	if err != nil || off+4 > len(query) {
		return nil
	}
	out := append([]byte(nil), query[:off+4]...)
	out[2] = 0x80 | (query[2] & 0x01) // QR, opcode 0, RD copied
	out[3] = 0x80 | byte(rcode&0x0f)  // RA
	binary.BigEndian.PutUint16(out[4:6], 1)
	binary.BigEndian.PutUint16(out[6:8], 0)
	binary.BigEndian.PutUint16(out[8:10], 0)
	binary.BigEndian.PutUint16(out[10:12], 0)
	return out
}
