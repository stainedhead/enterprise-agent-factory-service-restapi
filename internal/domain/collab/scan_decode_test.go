package collab

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/textproto"
	"strings"
	"testing"
	"unicode/utf16"
)

const awsKey = "AKIAABCDEFGHIJKLMNOP"

func utf16le(s string, bom bool) []byte {
	var out []byte
	if bom {
		out = append(out, 0xFF, 0xFE)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u&0xff), byte(u>>8))
	}
	return out
}

func utf16be(s string, bom bool) []byte {
	var out []byte
	if bom {
		out = append(out, 0xFE, 0xFF)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		out = append(out, byte(u>>8), byte(u&0xff))
	}
	return out
}

func multipartBody(t *testing.T, parts ...[3]string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="f"`)
		if p[0] != "" {
			h.Set("Content-Type", p[0])
		}
		if p[1] != "" {
			h.Set("Content-Transfer-Encoding", p[1])
		}
		pw, _ := w.CreatePart(h)
		_, _ = pw.Write([]byte(p[2]))
	}
	_ = w.Close()
	return "multipart/form-data; boundary=" + w.Boundary(), buf.Bytes()
}

func TestScanBodyBypasses(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte("key=" + awsKey))
	mpCT, mpB64 := multipartBody(t, [3]string{"text/plain", "base64", b64})
	qpCT, mpQP := multipartBody(t, [3]string{"text/plain", "quoted-printable", "x=3D" + "AKIA=\r\nABCDEFGHIJKLMNOP"})
	mpQP2 := strings.NewReplacer("name=\"f\"", "name=\"f\"").Replace(string(mpQP))
	nestCT, mpNested := multipartBody(t, [3]string{"application/octet-stream", "", "plain " + awsKey})
	tests := []struct {
		name, ct string
		body     []byte
		pattern  string
	}{
		{"plain", "text/plain", []byte(awsKey), PatternAWSAccessKey},
		{"json unicode escape in value", "application/json", []byte(`{"k":"AKIA\u0041BCDEFGHIJKLMNOP"}`), PatternAWSAccessKey},
		{"json escaped key", "application/json", []byte(`{"passw\u006frd":"hunter2hunter2hunter2"}`), PatternGenericAssignment},
		{"json surrogate pair", "application/json", []byte(`{"k":"\ud83d\ude00 AKIAABCDEFGHIJKLMNOP"}`), PatternAWSAccessKey},
		{"json escaped space", "application/json", []byte(`{"k":"-----BEGIN\u0020PRIVATE KEY-----"}`), PatternPrivateKey},
		{"form plus and percent", "application/x-www-form-urlencoded", []byte("a=1&password=hunter2hunter2"), PatternGenericAssignment},
		{"form percent-encoded name", "application/x-www-form-urlencoded", []byte("pass%77ord=hunter2hunter2"), PatternGenericAssignment},
		{"form percent-encoded key", "application/x-www-form-urlencoded", []byte("k=AKIA%41BCDEFGHIJKLMNOP"), PatternAWSAccessKey},
		{"form double encoded", "application/x-www-form-urlencoded", []byte("k=AKIA%2541BCDEFGHIJKLMNOP"), PatternAWSAccessKey},
		{"form plus as space", "application/x-www-form-urlencoded", []byte("k=-----BEGIN+PRIVATE+KEY-----"), PatternPrivateKey},
		{"multipart base64 part", mpCT, mpB64, PatternAWSAccessKey},
		{"multipart quoted-printable part", qpCT, []byte(mpQP2), PatternAWSAccessKey},
		{"multipart octet-stream text part", nestCT, mpNested, PatternAWSAccessKey},
		{"utf-16le BOM", "text/plain", utf16le(awsKey, true), PatternAWSAccessKey},
		{"utf-16be BOM", "text/plain", utf16be(awsKey, true), PatternAWSAccessKey},
		{"utf-16le declared", "text/plain; charset=utf-16le", utf16le(awsKey, false), PatternAWSAccessKey},
		{"utf-16be declared", "application/json; charset=UTF-16BE", utf16be(`{"k":"`+awsKey+`"}`, false), PatternAWSAccessKey},
		{"utf-16 declared no BOM defaults", "text/plain; charset=utf-16", utf16be(awsKey, false), PatternAWSAccessKey},
		{"utf-8 BOM", "text/plain", append([]byte{0xEF, 0xBB, 0xBF}, awsKey...), PatternAWSAccessKey},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, readable := ScanBody(tc.body, tc.ct)
			if !readable {
				t.Fatalf("body reported unreadable")
			}
			found := false
			for _, x := range m {
				if x.Pattern == tc.pattern {
					found = true
				}
			}
			if !found {
				t.Fatalf("pattern %s not found: %v", tc.pattern, m)
			}
		})
	}
}

func TestScanBodyClean(t *testing.T) {
	for _, ct := range []string{"application/json", "application/x-www-form-urlencoded", "text/plain", "text/plain; charset=utf-16"} {
		if m, ok := ScanBody([]byte(`{"title":"hello é world","n":1}`), ct); !ok || len(m) != 0 {
			t.Errorf("%s: clean body matched %v %v", ct, m, ok)
		}
	}
	ct, body := multipartBody(t, [3]string{"image/png", "", "\x89PNG\r\n\x1a\n\x00\x00binary" + string([]byte{0xff, 0xfe, 0xfd})})
	if m, ok := ScanBody(body, ct); !ok || len(m) != 0 {
		t.Errorf("binary part: %v %v", m, ok)
	}
}

func TestScanBodyUnreadable(t *testing.T) {
	if _, ok := ScanBody([]byte("x"), "text/plain; charset=utf-32"); ok {
		t.Error("utf-32 must be unreadable")
	}
	if _, ok := ScanBody([]byte("x"), "multipart/form-data"); ok {
		t.Error("multipart without boundary must be unreadable")
	}
	if _, ok := ScanBody([]byte("--b\r\nbroken"), "multipart/form-data; boundary=b"); ok {
		t.Error("broken multipart must be unreadable")
	}
}

func TestScanMessage(t *testing.T) {
	if m := ScanMessage([]byte(`{"k":"AKIA\u0041BCDEFGHIJKLMNOP"}`)); len(m) == 0 {
		t.Error("escaped key in a message not found")
	}
	if m := ScanMessage([]byte{0xff, 0xfe, 0x00, 0x01}); len(m) != 0 {
		t.Error("binary garbage must not match")
	}
}

func TestBodyKinds(t *testing.T) {
	tests := []struct {
		mt   string
		want BodyKind
	}{
		{"application/json", BodyScan}, {"", BodyScan}, {"text/csv", BodyScan}, {"multipart/form-data", BodyScan},
		{"application/x-www-form-urlencoded", BodyScan}, {"application/vnd.api+json", BodyScan}, {"application/xml", BodyScan},
		{"image/png", BodyBinaryAllowed}, {"video/mp4", BodyBinaryAllowed}, {"audio/mpeg", BodyBinaryAllowed},
		{"application/pdf", BodyBinaryAllowed},
		{"application/octet-stream", BodyUnsupported}, {"application/zip", BodyUnsupported}, {"application/x-msgpack", BodyUnsupported},
		{"application/protobuf", BodyUnsupported}, {"application/cbor", BodyUnsupported},
	}
	for _, tc := range tests {
		if got := ClassifyBody(tc.mt); got != tc.want {
			t.Errorf("%q = %v, want %v", tc.mt, got, tc.want)
		}
	}
}

func TestUnescapeJSONForms(t *testing.T) {
	for in, want := range map[string]string{
		`a\nb\tc\rd\be\ff`: "a\nb\tc\rd\be\ff", `\/x\"y\\z`: `/x"y\z`, `\u0041`: "A", `\ud83d\ude00`: "\U0001F600",
		`\ud83dx`: "\ufffdx", `\u00zz`: "u00zz", `\q`: "q", `trail\`: `trail\`, `\u12`: "u12",
		`\u00e9`: "\u00e9",
	} {
		got := unescapeJSON([]byte(in))
		if string(got) != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if unescapeJSON([]byte("no escapes")) != nil {
		t.Error("nothing to decode must return nil")
	}
}

func TestPercentDecodeLenient(t *testing.T) {
	for in, want := range map[string]string{"a%41+b": "aA b", "%zz": "%zz", "%4": "%4", "100%": "100%", "%e9": "\xe9", "%%41": "%A"} {
		if got := string(percentDecode([]byte(in), true)); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if got := string(percentDecode([]byte("a+b"), false)); got != "a+b" {
		t.Errorf("plus kept: %q", got)
	}
}

func TestDecodeBase64Variants(t *testing.T) {
	for name, in := range map[string]string{"std": "aGVsbG8=", "raw": "aGVsbG8", "wrapped": "aGVs\r\nbG8=", "url": "aGVsbG8="} {
		if got, ok := decodeBase64([]byte(in)); !ok || string(got) != "hello" {
			t.Errorf("%s: %q %v", name, got, ok)
		}
	}
	if _, ok := decodeBase64([]byte("!!!")); ok {
		t.Error("garbage")
	}
}

func TestMultipartEdgeCases(t *testing.T) {
	// base64 that does not decode is scanned as it is
	ct, body := multipartBody(t, [3]string{"text/plain", "base64", "!!! " + awsKey})
	if m, ok := ScanBody(body, ct); !ok || len(m) == 0 {
		t.Errorf("undecodable base64 part: %v %v", m, ok)
	}
	// a part in a charset we cannot read still gets a raw scan
	ct, body = multipartBody(t, [3]string{"text/plain; charset=utf-32", "", awsKey})
	if m, ok := ScanBody(body, ct); !ok || len(m) == 0 {
		t.Errorf("unreadable part charset: %v %v", m, ok)
	}
	// nested multipart is followed, up to a depth
	inner, ib := multipartBody(t, [3]string{"text/plain", "", awsKey})
	ct, body = multipartBody(t, [3]string{inner, "", string(ib)})
	if m, ok := ScanBody(body, ct); !ok || len(m) == 0 {
		t.Errorf("nested: %v %v", m, ok)
	}
	for range 4 {
		ct, body = multipartBody(t, [3]string{ct, "", string(body)})
	}
	if _, ok := ScanBody(body, ct); ok {
		t.Error("multipart nested beyond the limit must be unreadable")
	}
	// content type that does not parse
	if m, ok := ScanBody([]byte(awsKey), ";;;"); !ok || len(m) == 0 {
		t.Errorf("unparsable content type: %v %v", m, ok)
	}
	// image part inside multipart is skipped, top-level image is not scanned
	if m, ok := ScanBody([]byte(awsKey), "image/png"); !ok || len(m) != 0 {
		t.Errorf("image: %v %v", m, ok)
	}
}

func TestScanViewsCountsAndOrder(t *testing.T) {
	m := ScanMessage([]byte(awsKey + " " + awsKey + ` -----BEGIN PRIVATE KEY-----`))
	if len(m) != 2 || m[0].Pattern != PatternAWSAccessKey || m[0].Count != 2 || m[1].Pattern != PatternPrivateKey {
		t.Errorf("%v", m)
	}
}
