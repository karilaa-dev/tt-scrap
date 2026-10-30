package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"

	"github.com/karilaa-dev/tt-scrap/internal/core"
	"github.com/karilaa-dev/tt-scrap/internal/instagramapp"
)

func readInstagramImport(w http.ResponseWriter, r *http.Request) ([]instagramapp.Credentials, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	kind, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil {
		return nil, invalidRequest()
	}
	switch kind {
	case "text/plain":
		return instagramapp.ParseCookieExport(r.Body)
	case "text/csv":
		return instagramapp.ParseCSV(r.Body)
	case "multipart/form-data":
		// The entire bounded upload stays in memory; credentials never spill to disk.
		if e = r.ParseMultipartForm(2 << 20); e != nil {
			return nil, invalidRequest()
		}
		defer r.MultipartForm.RemoveAll()
		if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 || len(r.MultipartForm.Value) != 0 {
			return nil, invalidRequest()
		}
		file, e := r.MultipartForm.File["file"][0].Open()
		if e != nil {
			return nil, invalidRequest()
		}
		defer file.Close()
		return instagramapp.ParseImportFile(file)
	case "application/json":
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, invalidRequest()
		}
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			if !json.Valid(raw) {
				return nil, invalidRequest()
			}
			return instagramapp.ParseCookieExport(bytes.NewReader(raw))
		}
		var body struct {
			Accounts []instagramapp.Credentials `json:"accounts"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
			return nil, invalidRequest()
		}
		return body.Accounts, nil
	default:
		return nil, core.NewError("unsupported_media_type", "Use text/plain cookie exports, text/csv, multipart/form-data with file, or application/json", 415)
	}
}
