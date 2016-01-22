package http2

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
)

type Handler func(*Conn)

type Server struct {
	Addr    string
	Handler Handler
	Config  *Config
}

func ServerConn(rawConn net.Conn, config *Config) *Conn {
	return newConn(rawConn, true, config)
}

func (c *Conn) serverHandshake() error {
	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		if !tlsConn.ConnectionState().HandshakeComplete {
			if err := tlsConn.Handshake(); err != nil {
				return err
			}
		}

		state := tlsConn.ConnectionState()

		if state.NegotiatedProtocol != VersionTLS {
			return fmt.Errorf("bad protocol %s", state.NegotiatedProtocol)
		}

		// Due to deployment limitations, it might not
		// be possible to fail TLS negotiation when these restrictions are not
		// met.  An endpoint MAY immediately terminate an HTTP/2 connection that
		// does not meet these TLS requirements with a connection error
		// (Section 5.4.1) of type INADEQUATE_SECURITY.
		if !c.config.AllowLowTLSVersion && state.Version < tls.VersionTLS12 {
			return ConnError{fmt.Errorf("bad TLS version %x", state.Version), ErrCodeInadequateSecurity}
		}

		// A deployment of HTTP/2 over TLS 1.2 SHOULD NOT use any of the cipher
		// suites that are listed in the cipher suite black list (Appendix A).
		//
		// Endpoints MAY choose to generate a connection error (Section 5.4.1)
		// of type INADEQUATE_SECURITY if one of the cipher suites from the
		// black list is negotiated.
		if state.Version >= tls.VersionTLS12 && badCipher(state.CipherSuite) {
			return ConnError{fmt.Errorf("prohibited TLS 1.2 cipher type %x", state.CipherSuite), ErrCodeInadequateSecurity}
		}
	} else {
		upgradeFunc := c.upgradeFunc
		if upgradeFunc == nil {
			upgradeFunc = func() error {
				upgrade, err := http.ReadRequest(c.buf.Reader)
				if err == nil {
					err = c.serverUpgrade(upgrade, false)
				}
				return err
			}
		}
		if err := upgradeFunc(); err != nil {
			return err
		}
		c.upgradeFunc = nil
	}

	// The server connection preface consists of a potentially empty
	// SETTINGS frame (Section 6.5) that MUST be the first frame the server
	// sends in the HTTP/2 connection.
	if err := c.writeFrame(&SettingsFrame{false, c.config.InitialSettings}); err != nil {
		return err
	}

	// The client connection preface starts with a sequence of 24 octets.
	// This sequence MUST be followed by a
	// SETTINGS frame (Section 6.5), which MAY be empty.
	if preface, err := c.buf.Peek(len(ClientPreface)); err == nil {
		if !bytes.Equal(preface, clientPreface) {
			return errBadConnPreface
		}
	} else {
		return err
	}
	c.buf.Discard(len(ClientPreface))
	if firstFrame, err := c.readFrame(); err == nil {
		if settings, ok := firstFrame.(*SettingsFrame); !ok || settings.Ack {
			return errors.New("first received frame was not SETTINGS")
		}
	} else {
		return err
	}

	return nil
}

func (c *Conn) serverUpgrade(upgrade *http.Request, hijacked bool) error {
	status := http.StatusBadRequest
	reason := "bad upgrade request"

	if upgrade.Method != "GET" {
		status = http.StatusMethodNotAllowed
		goto fail
	}

	// The client does so by
	// making an HTTP/1.1 request that includes an Upgrade header field with
	// the "h2c" token.
	if !containsValue(upgrade.Header, "Upgrade", VersionTCP) {
		goto fail
	}

	// Since the upgrade is only intended to apply to the immediate
	// connection, a client sending the HTTP2-Settings header field MUST
	// also send "HTTP2-Settings" as a connection option in the Connection
	// header field to prevent it from being forwarded (see Section 6.1 of
	// [RFC7230]).
	if !containsValue(upgrade.Header, "Connection", "Upgrade", "HTTP2-Settings") {
		goto fail
	}

	// A request that upgrades from HTTP/1.1 to HTTP/2 MUST include exactly
	// one "HTTP2-Settings" header field.
	//
	// A server MUST NOT upgrade the connection to HTTP/2 if this header
	// field is not present or if more than one is present.  A server MUST
	// NOT send this header field.
	if values := splitHeader(upgrade.Header, "HTTP2-Settings"); len(values) != 1 {
		goto fail
	} else {
		// The content of the HTTP2-Settings header field is the payload of a
		// SETTINGS frame (Section 6.5), encoded as a base64url string (that is,
		// the URL- and filename-safe Base64 encoding described in Section 5 of
		// [RFC4648], with any trailing '=' characters omitted).
		//
		// A server decodes and interprets these values as it would any other
		// SETTINGS frame.  Explicit acknowledgement of these settings
		// (Section 6.5.3) is not necessary, since a 101 response serves as
		// implicit acknowledgement.
		payload, err := base64.URLEncoding.DecodeString(values[0])
		if err != nil {
			reason = err.Error()
			goto fail
		}
		payloadLen := len(payload)
		if payloadLen%settingLen != 0 {
			goto fail
		}
		var settings Settings
		for i := 0; i < payloadLen/settingLen; i++ {
			id := SettingID(binary.BigEndian.Uint16(payload[:2]))
			value := binary.BigEndian.Uint32(payload[2:6])
			if err = settings.SetValue(id, value); err != nil {
				reason = err.Error()
				goto fail
			}
			payload = payload[settingLen:]
		}
		if err = c.remote.applySettings(settings); err != nil {
			reason = err.Error()
			goto fail
		}
	}

	status = http.StatusOK

fail:
	if status != http.StatusOK {
		fmt.Fprintf(c.buf, "HTTP/1.1 %03d %s\r\n", status, http.StatusText(status))
		c.buf.WriteString("\r\n")
		c.buf.WriteString(reason)
		c.buf.Flush()

		return upgradeError(reason)
	}

	// The HTTP/1.1 request that is sent prior to upgrade is assigned a
	// stream identifier of 1 (see Section 5.1.1) with default priority
	// values (Section 5.3.5).  Stream 1 is implicitly "half-closed" from
	// the client toward the server (see Section 5.1), since the request is
	// completed as an HTTP/1.1 request.  After commencing the HTTP/2
	// connection, stream 1 is used for the response.
	stream, _ := c.remote.idleStream(1)
	stream.transition(true, FrameHeaders, true)

	if !hijacked {
		headers := &HeadersFrame{1, Header(upgrade.Header), Priority{}, 0, upgrade.Body == nil}
		headers.SetMethod(upgrade.Method)
		headers.SetAuthority(upgrade.Host)
		headers.SetPath(upgrade.URL.Path)
		headers.SetScheme("http")
		c.upgradeFrames = append(c.upgradeFrames, headers)
		if upgrade.Body != nil {
			c.upgradeFrames = append(c.upgradeFrames, &DataFrame{1, upgrade.Body, int(upgrade.ContentLength), 0, true})
		}
	}

	c.buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	c.buf.WriteString("Connection: Upgrade\r\n")
	c.buf.WriteString("Upgrade: h2c\r\n\r\n")
	c.buf.Flush()

	return nil
}

func badCipher(cipher uint16) bool {
	switch cipher {
	case
		tls.TLS_RSA_WITH_RC4_128_SHA,
		tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_RC4_128_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_RC4_128_SHA,
		tls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA:

		return true
	default:
		return false
	}
}
