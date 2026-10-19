package proxy

// The ClientHello peek of a CONNECT tunnel. The proxy never decrypts a
// tunnel; it reads the client's first TLS handshake message, which is
// plaintext in every TLS version, to compare the server_name it carries with
// the host the CONNECT named. That closes classic domain fronting: a run that
// may reach allowed.example cannot CONNECT to allowed.example and then ask a
// shared front end, through the SNI, for another name.
//
// Encrypted Client Hello (ECH): the ClientHello on the wire is the OUTER one.
// Its server_name is the public name of the ECH configuration, not the real
// destination, which travels in the encrypted inner ClientHello. The proxy
// checks the outer name, so an ECH client whose public name is not the
// CONNECT host is refused. An ECH client whose public name IS the CONNECT
// host passes, and its inner name stays out of the proxy's sight, as the
// HTTP Host header inside the tunnel does.

import "errors"

// The TLS wire constants the peek needs (RFC 8446, RFC 6066, RFC 9849).
const (
	recordTypeHandshake      = 0x16
	recordHeaderLen          = 5
	maxRecordPayload         = 1 << 14 // the TLSPlaintext.length limit
	handshakeTypeClientHello = 0x01
	handshakeHeaderLen       = 4

	extensionServerName = 0x0000
	extensionECH        = 0xfe0d
	serverNameHostName  = 0x00
)

var (
	// errNotTLS: the first byte is not a TLS handshake record.
	errNotTLS = errors.New("the first bytes are not a TLS handshake record")
	// errHelloMalformed: the bytes start as TLS but are not a well-formed
	// ClientHello. The proxy refuses rather than guess, so a parser
	// difference with the destination cannot hide a name.
	errHelloMalformed = errors.New("the ClientHello is malformed")
	// errHelloTooLarge: the ClientHello cannot fit in the peek bound.
	errHelloTooLarge = errors.New("the ClientHello is larger than the peek bound")
)

// clientHello is what the peek learns from a ClientHello.
type clientHello struct {
	serverName    string
	hasServerName bool
	// hasECH: the hello carries an encrypted_client_hello extension, so
	// serverName is the ECH public name (the outer SNI).
	hasECH bool
}

// scanClientHello reads the TLS records at the start of buf, the bytes the
// client sent so far, and reassembles the handshake bytes they carry. It
// returns complete=true with the parsed hello once the whole ClientHello
// message is in buf; complete=false and no error when more bytes are needed;
// or one of errNotTLS, errHelloMalformed and errHelloTooLarge. maxBytes
// bounds the bytes the whole ClientHello, record headers included, may take.
//
// A ClientHello may span several handshake records (RFC 8446 5.1); they must
// be consecutive, with no other record type between them. Bytes after the
// ClientHello are not looked at. The function never panics on any input.
func scanClientHello(buf []byte, maxBytes int) (hello clientHello, complete bool, err error) {
	if len(buf) == 0 {
		return clientHello{}, false, nil
	}
	if buf[0] != recordTypeHandshake {
		return clientHello{}, false, errNotTLS
	}
	var hs []byte
	off := 0
	for {
		rest := buf[off:]
		if len(rest) == 0 {
			return clientHello{}, false, nil
		}
		if rest[0] != recordTypeHandshake {
			// An alert, change_cipher_spec or application data before the
			// ClientHello is complete.
			return clientHello{}, false, errHelloMalformed
		}
		if len(rest) >= 2 && rest[1] != 0x03 {
			// legacy_record_version is 0x03xx in SSL 3.0 and every TLS.
			return clientHello{}, false, errHelloMalformed
		}
		if len(rest) < recordHeaderLen {
			return clientHello{}, false, nil
		}
		n := int(rest[3])<<8 | int(rest[4])
		if n == 0 || n > maxRecordPayload {
			// A zero-length handshake fragment is forbidden (RFC 8446 5.1).
			return clientHello{}, false, errHelloMalformed
		}
		if off+recordHeaderLen+n > maxBytes {
			return clientHello{}, false, errHelloTooLarge
		}
		if len(rest) < recordHeaderLen+n {
			return clientHello{}, false, nil
		}
		hs = append(hs, rest[recordHeaderLen:recordHeaderLen+n]...)
		off += recordHeaderLen + n

		if len(hs) < handshakeHeaderLen {
			continue
		}
		if hs[0] != handshakeTypeClientHello {
			return clientHello{}, false, errHelloMalformed
		}
		bodyLen := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
		if handshakeHeaderLen+bodyLen > maxBytes {
			return clientHello{}, false, errHelloTooLarge
		}
		if len(hs) < handshakeHeaderLen+bodyLen {
			continue
		}
		hello, err := parseClientHello(hs[handshakeHeaderLen : handshakeHeaderLen+bodyLen])
		if err != nil {
			return clientHello{}, false, err
		}
		return hello, true, nil
	}
}

// byteReader is a bounds-checked cursor over a byte slice. A read past the
// end marks it bad and returns zero values; callers check ok() once.
type byteReader struct {
	b   []byte
	bad bool
}

func (r *byteReader) ok() bool { return !r.bad }

func (r *byteReader) take(n int) []byte {
	if r.bad || n < 0 || n > len(r.b) {
		r.bad = true
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *byteReader) u8() int {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return int(b[0])
}

func (r *byteReader) u16() int {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return int(b[0])<<8 | int(b[1])
}

// vec8 and vec16 read a length-prefixed vector.
func (r *byteReader) vec8() []byte  { return r.take(r.u8()) }
func (r *byteReader) vec16() []byte { return r.take(r.u16()) }

// parseClientHello parses a ClientHello body (RFC 8446 4.1.2, the handshake
// header removed). It is strict: trailing bytes, a duplicated extension or a
// server_name list with more than one host name is malformed.
func parseClientHello(body []byte) (clientHello, error) {
	r := &byteReader{b: body}
	r.take(2)  // legacy_version
	r.take(32) // random
	if sid := r.vec8(); len(sid) > 32 {
		return clientHello{}, errHelloMalformed
	}
	if suites := r.vec16(); len(suites) < 2 || len(suites)%2 != 0 {
		return clientHello{}, errHelloMalformed
	}
	if comp := r.vec8(); len(comp) < 1 {
		return clientHello{}, errHelloMalformed
	}
	if !r.ok() {
		return clientHello{}, errHelloMalformed
	}
	var hello clientHello
	if len(r.b) == 0 {
		// No extensions block at all (TLS 1.2 and older allow it).
		return hello, nil
	}
	exts := &byteReader{b: r.vec16()}
	if !r.ok() || len(r.b) != 0 {
		return clientHello{}, errHelloMalformed
	}
	seen := map[int]bool{}
	for len(exts.b) > 0 {
		typ := exts.u16()
		data := exts.vec16()
		if !exts.ok() || seen[typ] {
			return clientHello{}, errHelloMalformed
		}
		seen[typ] = true
		switch typ {
		case extensionServerName:
			name, err := parseServerName(data)
			if err != nil {
				return clientHello{}, err
			}
			hello.serverName, hello.hasServerName = name, true
		case extensionECH:
			hello.hasECH = true
		}
	}
	return hello, nil
}

// parseServerName parses the server_name extension data (RFC 6066 3) and
// returns its one host_name.
func parseServerName(data []byte) (string, error) {
	r := &byteReader{b: data}
	list := &byteReader{b: r.vec16()}
	if !r.ok() || len(r.b) != 0 || len(list.b) == 0 {
		return "", errHelloMalformed
	}
	var name []byte
	for len(list.b) > 0 {
		typ := list.u8()
		value := list.vec16()
		if !list.ok() {
			return "", errHelloMalformed
		}
		// RFC 6066 defines one name type and allows one name of it; anything
		// else is refused rather than guessed at.
		if typ != serverNameHostName || name != nil || len(value) == 0 {
			return "", errHelloMalformed
		}
		name = value
	}
	return string(name), nil
}

// sameHost compares an SNI with the CONNECT host: ASCII case-insensitive,
// with one trailing dot ignored on either side. Only ASCII letters fold; any
// other byte must match exactly, so a Unicode fold (the Kelvin sign against
// "k") never makes two names equal.
func sameHost(sni, host string) bool {
	sni, host = trimOneDot(sni), trimOneDot(host)
	if sni == "" || len(sni) != len(host) {
		return false
	}
	for i := 0; i < len(sni); i++ {
		if lowerASCII(sni[i]) != lowerASCII(host[i]) {
			return false
		}
	}
	return true
}

func trimOneDot(s string) string {
	if s != "" && s[len(s)-1] == '.' {
		return s[:len(s)-1]
	}
	return s
}

func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
