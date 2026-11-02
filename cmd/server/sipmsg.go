package main

// Minimal SIP message handling for the SIP gateway (sipgateway.go): just what
// one phone talking to us over one TCP connection needs. Not a general SIP
// stack: no UDP retransmissions, no forking, no proxying.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

type sipHeader struct{ name, value string }

type sipMsg struct {
	// Request: Method + URI. Response: Code + Reason (Method empty).
	Method  string
	URI     string
	Code    int
	Reason  string
	Headers []sipHeader
	Body    []byte
}

var sipCompact = map[string]string{
	"v": "via", "f": "from", "t": "to", "i": "call-id", "m": "contact",
	"l": "content-length", "c": "content-type", "k": "supported", "e": "content-encoding",
}

func sipCanon(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if full, ok := sipCompact[n]; ok {
		return full
	}
	return n
}

func (m *sipMsg) isRequest() bool { return m.Method != "" }

// Get returns the first value of a header (case-insensitive, compact forms too).
func (m *sipMsg) Get(name string) string {
	c := sipCanon(name)
	for _, h := range m.Headers {
		if sipCanon(h.name) == c {
			return h.value
		}
	}
	return ""
}

// All returns every value of a header, splitting comma lists for Via.
func (m *sipMsg) All(name string) []string {
	c := sipCanon(name)
	var out []string
	for _, h := range m.Headers {
		if sipCanon(h.name) != c {
			continue
		}
		if c == "via" {
			for _, p := range strings.Split(h.value, ",") {
				out = append(out, strings.TrimSpace(p))
			}
			continue
		}
		out = append(out, h.value)
	}
	return out
}

func (m *sipMsg) Set(name, value string) {
	c := sipCanon(name)
	for i, h := range m.Headers {
		if sipCanon(h.name) == c {
			m.Headers[i].value = value
			return
		}
	}
	m.Add(name, value)
}

func (m *sipMsg) Add(name, value string) { m.Headers = append(m.Headers, sipHeader{name, value}) }

// CSeq returns the number and method of the CSeq header.
func (m *sipMsg) CSeq() (int, string) {
	f := strings.Fields(m.Get("cseq"))
	if len(f) != 2 {
		return 0, ""
	}
	n, _ := strconv.Atoi(f[0])
	return n, strings.ToUpper(f[1])
}

func (m *sipMsg) Bytes() []byte {
	var b bytes.Buffer
	if m.isRequest() {
		fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", m.Method, m.URI)
	} else {
		fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", m.Code, m.Reason)
	}
	for _, h := range m.Headers {
		if sipCanon(h.name) == "content-length" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\r\n", h.name, h.value)
	}
	fmt.Fprintf(&b, "Content-Length: %d\r\n\r\n", len(m.Body))
	b.Write(m.Body)
	return b.Bytes()
}

var errSIPIncomplete = errors.New("sip: incomplete message")

// parseSIP reads one message from the start of buf (stream transport). It
// returns the message and the bytes it used, or errSIPIncomplete.
func parseSIP(buf []byte) (*sipMsg, int, error) {
	end := bytes.Index(buf, []byte("\r\n\r\n"))
	if end < 0 {
		return nil, 0, errSIPIncomplete
	}
	lines := strings.Split(string(buf[:end]), "\r\n")
	m := &sipMsg{}
	first := lines[0]
	if strings.HasPrefix(first, "SIP/2.0 ") {
		parts := strings.SplitN(first, " ", 3)
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, 0, fmt.Errorf("sip: bad status line %q", first)
		}
		m.Code = code
		if len(parts) > 2 {
			m.Reason = parts[2]
		}
	} else {
		parts := strings.Fields(first)
		if len(parts) != 3 || parts[2] != "SIP/2.0" {
			return nil, 0, fmt.Errorf("sip: bad request line %q", first)
		}
		m.Method, m.URI = strings.ToUpper(parts[0]), parts[1]
	}
	for _, l := range lines[1:] {
		if l == "" {
			continue
		}
		if (l[0] == ' ' || l[0] == '\t') && len(m.Headers) > 0 { // folded line
			m.Headers[len(m.Headers)-1].value += " " + strings.TrimSpace(l)
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		m.Headers = append(m.Headers, sipHeader{strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	n := 0
	if cl := m.Get("content-length"); cl != "" {
		var err error
		if n, err = strconv.Atoi(strings.TrimSpace(cl)); err != nil || n < 0 {
			return nil, 0, fmt.Errorf("sip: bad Content-Length %q", cl)
		}
	}
	total := end + 4 + n
	if len(buf) < total {
		return nil, 0, errSIPIncomplete
	}
	m.Body = append([]byte(nil), buf[end+4:total]...)
	return m, total, nil
}

// sipResponse builds a response to req, copying the headers RFC 3261 §8.2.6 asks for.
func sipResponse(req *sipMsg, code int, reason, toTag string) *sipMsg {
	r := &sipMsg{Code: code, Reason: reason}
	for _, v := range req.All("via") {
		r.Add("Via", v)
	}
	r.Add("From", req.Get("from"))
	to := req.Get("to")
	if toTag != "" && sipParam(to, "tag") == "" {
		to += ";tag=" + toTag
	}
	r.Add("To", to)
	r.Add("Call-ID", req.Get("call-id"))
	r.Add("CSeq", req.Get("cseq"))
	return r
}

// sipParam returns a ;name=value parameter of a header value (outside <...>).
func sipParam(v, name string) string {
	if i := strings.LastIndex(v, ">"); i >= 0 {
		v = v[i+1:]
	}
	for _, p := range strings.Split(v, ";")[1:] {
		k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		if strings.EqualFold(k, name) {
			return val
		}
	}
	return ""
}

// sipAddrURI returns the URI inside a name-addr ("Name" <sip:..>) or the bare URI.
func sipAddrURI(v string) string {
	if i := strings.Index(v, "<"); i >= 0 {
		if j := strings.Index(v[i:], ">"); j > 0 {
			return v[i+1 : i+j]
		}
	}
	if i := strings.Index(v, ";"); i >= 0 {
		return strings.TrimSpace(v[:i])
	}
	return strings.TrimSpace(v)
}

// sipURIUser returns the (unescaped) user part of sip:user@host;params.
func sipURIUser(uri string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(uri, "sips:"), "sip:")
	at := strings.Index(u, "@")
	if at < 0 {
		return ""
	}
	if s, err := url.PathUnescape(u[:at]); err == nil {
		return s
	}
	return u[:at]
}

func sipRandom(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sipBranch() string { return "z9hG4bK" + sipRandom(8) }
