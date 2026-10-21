package zapi

import (
	"bytes"
	"io"
	"mime/multipart"
	"strings"
	"testing"
)

func multipartBody(boundary string, fields map[string]string, file string) []byte {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.SetBoundary(boundary)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", "f.bin")
	_, _ = fw.Write([]byte(file))
	_ = mw.Close()
	return buf.Bytes()
}

func TestFilePartFindsFileAfterFields(t *testing.T) {
	body := multipartBody("BOUND", map[string]string{"key": "abc", "x": strings.Repeat("y", 5000)}, "payload")
	r, err := filePart(bytes.NewReader(body), "BOUND")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != "payload" {
		t.Fatalf("got %q", got)
	}
}

func FuzzFilePart(f *testing.F) {
	f.Add(multipartBody("B", map[string]string{"key": "k"}, "data"), "B")
	f.Add([]byte("--B\r\nContent-Disposition: form-data; name=\"file\"\r\n\r\nx\r\n--B--"), "B")
	f.Add([]byte("--B\r\n\r\n"), "B")
	f.Add([]byte{}, "")
	f.Fuzz(func(t *testing.T, body []byte, boundary string) {
		r, err := filePart(bytes.NewReader(body), boundary)
		if err != nil {
			return
		}
		// Whatever the shape of the input, the part can never yield more
		// than the body held.
		n, _ := io.Copy(io.Discard, r)
		if n > int64(len(body)) {
			t.Fatalf("part longer (%d) than body (%d)", n, len(body))
		}
	})
}
