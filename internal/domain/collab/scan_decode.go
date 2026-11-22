package collab

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// BodyKind says what the edge does with a request body of a media type.
type BodyKind int

// Body kinds.
const (
	// BodyScan is a media type the scanner can read (it decodes JSON escapes, form encoding, multipart parts
	// and UTF-16 before matching).
	BodyScan BodyKind = iota
	// BodyBinaryAllowed is a listed binary upload that is passed through unscanned: images, audio, video and
	// PDF, which Paperclip attachments need (RD-25).
	BodyBinaryAllowed
	// BodyUnsupported is any other media type: a non-safe request with it is refused with 415, because the
	// scanner cannot read it and a secret could be hidden in it.
	BodyUnsupported
)

// ClassifyBody classifies a lower-case media type without parameters.
func ClassifyBody(mediaType string) BodyKind {
	switch {
	case TextContentType(mediaType), strings.HasPrefix(mediaType, "multipart/"):
		return BodyScan
	case strings.HasPrefix(mediaType, "image/"), strings.HasPrefix(mediaType, "audio/"), strings.HasPrefix(mediaType, "video/"),
		mediaType == "application/pdf":
		return BodyBinaryAllowed
	}
	switch mediaType {
	case "application/javascript", "application/x-ndjson", "application/graphql", "application/csv":
		return BodyScan
	}
	return BodyUnsupported
}

const maxMultipartDepth = 3

// ScanBody scans a request body after decoding it the way a receiver would: the declared or BOM-marked
// charset (UTF-8, ASCII, Latin-1 as bytes; UTF-16 decoded), JSON string escapes, form urlencoding, and
// multipart parts with their Content-Transfer-Encoding. readable is false when the body cannot be decoded
// (an unknown charset, or a malformed multipart): the caller refuses it. The matched text is never kept.
func ScanBody(body []byte, contentType string) (matches []Match, readable bool) {
	views, ok := bodyViews(body, contentType, 0)
	if !ok {
		return nil, false
	}
	return scanViews(views), true
}

// ScanMessage scans one WebSocket message: the text as sent and with JSON string escapes decoded.
func ScanMessage(b []byte) []Match {
	views := [][]byte{b}
	if v := unescapeJSON(b); v != nil {
		views = append(views, v)
	}
	return scanViews(views)
}

func scanViews(views [][]byte) []Match {
	totals := map[string]int{}
	for _, v := range views {
		st := NewStream()
		st.Write(v)
		for _, m := range st.Finish() {
			totals[m.Pattern] = max(totals[m.Pattern], m.Count)
		}
	}
	var out []Match
	for _, p := range PatternTypes() {
		if n := totals[p]; n > 0 {
			out = append(out, Match{Pattern: p, Count: n})
		}
	}
	return out
}

func bodyViews(body []byte, contentType string, depth int) ([][]byte, bool) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mt, params = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])), nil
	}
	if ClassifyBody(mt) == BodyBinaryAllowed {
		return nil, true
	}
	texts, ok := decodeCharset(body, params["charset"])
	if !ok {
		return nil, false
	}
	var views [][]byte
	for _, text := range texts {
		views = append(views, text)
		switch {
		case strings.HasPrefix(mt, "multipart/"):
			sub, ok := multipartViews(text, params["boundary"], depth)
			if !ok {
				return nil, false
			}
			views = append(views, sub...)
		case mt == "application/x-www-form-urlencoded":
			cur := text
			for range 2 {
				cur = percentDecode(cur, true)
				views = append(views, cur)
			}
		default:
			if v := unescapeJSON(text); v != nil {
				views = append(views, v)
			}
		}
	}
	return views, true
}

// decodeCharset returns the body as UTF-8 text candidates, or ok=false for a charset it cannot read.
func decodeCharset(b []byte, charset string) ([][]byte, bool) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return [][]byte{b[3:]}, true
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return [][]byte{decodeUTF16(b[2:], false)}, true
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return [][]byte{decodeUTF16(b[2:], true)}, true
	}
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "utf-8", "utf8", "us-ascii", "ascii", "iso-8859-1", "latin1", "windows-1252":
		return [][]byte{b}, true
	case "utf-16le":
		return [][]byte{decodeUTF16(b, false)}, true
	case "utf-16be":
		return [][]byte{decodeUTF16(b, true)}, true
	case "utf-16":
		// No BOM and no byte order: scan both (RFC 2781 says big-endian, but senders differ).
		return [][]byte{decodeUTF16(b, true), decodeUTF16(b, false)}, true
	}
	return nil, false
}

func decodeUTF16(b []byte, bigEndian bool) []byte {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	return []byte(string(utf16.Decode(u)))
}

func multipartViews(body []byte, boundary string, depth int) ([][]byte, bool) {
	if boundary == "" || depth >= maxMultipartDepth {
		return nil, false
	}
	var views [][]byte
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		p, err := mr.NextPart() // decodes quoted-printable
		if errors.Is(err, io.EOF) {
			return views, true
		}
		if err != nil {
			return nil, false
		}
		data, err := io.ReadAll(p)
		if err != nil {
			return nil, false
		}
		if strings.EqualFold(strings.TrimSpace(p.Header.Get("Content-Transfer-Encoding")), "base64") {
			if dec, ok := decodeBase64(data); ok {
				data = dec
			}
		}
		pct := p.Header.Get("Content-Type")
		sub, ok := bodyViews(data, pct, depth+1)
		if !ok {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(pct)), "multipart/") {
				return nil, false // a nested multipart that is malformed or too deep cannot be read
			}
			// A part in a charset we cannot read still gets a raw scan; only the top level is refused.
			sub = [][]byte{data}
		}
		views = append(views, sub...)
	}
}

func decodeBase64(b []byte) ([]byte, bool) {
	clean := make([]byte, 0, len(b))
	for _, c := range b {
		if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
			clean = append(clean, c)
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if out, err := enc.DecodeString(string(clean)); err == nil {
			return out, true
		}
	}
	return nil, false
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// percentDecode decodes %XX (and + as space when plus is set); a malformed escape is kept as it is.
func percentDecode(b []byte, plus bool) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '+' && plus:
			out = append(out, ' ')
		case b[i] == '%' && i+2 < len(b):
			h, ok1 := hexVal(b[i+1])
			l, ok2 := hexVal(b[i+2])
			if ok1 && ok2 {
				out = append(out, h<<4|l)
				i += 2
				continue
			}
			out = append(out, b[i])
		default:
			out = append(out, b[i])
		}
	}
	return out
}

// unescapeJSON decodes JSON string escapes (\uXXXX with surrogate pairs, \n, \/ ...) anywhere in b, without
// parsing, so a body that is not valid JSON is decoded too. It returns nil when there is nothing to decode.
func unescapeJSON(b []byte) []byte {
	if bytes.IndexByte(b, '\\') < 0 {
		return nil
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			out = append(out, b[i])
			continue
		}
		i++
		switch b[i] {
		case 'n':
			out = append(out, '\n')
		case 't':
			out = append(out, '\t')
		case 'r':
			out = append(out, '\r')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'u':
			r, n := hex4(b[i+1:])
			if n == 0 {
				out = append(out, 'u')
				continue
			}
			i += n
			if utf16.IsSurrogate(r) && i+6 < len(b) && b[i+1] == '\\' && b[i+2] == 'u' {
				if r2, n2 := hex4(b[i+3:]); n2 > 0 {
					if dec := utf16.DecodeRune(r, r2); dec != utf8.RuneError {
						out = utf8.AppendRune(out, dec)
						i += 2 + n2
						continue
					}
				}
			}
			out = utf8.AppendRune(out, r)
		default:
			out = append(out, b[i])
		}
	}
	return out
}

func hex4(b []byte) (rune, int) {
	if len(b) < 4 {
		return 0, 0
	}
	var r rune
	for i := 0; i < 4; i++ {
		v, ok := hexVal(b[i])
		if !ok {
			return 0, 0
		}
		r = r<<4 | rune(v)
	}
	return r, 4
}
